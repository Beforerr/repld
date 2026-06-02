using InteractiveUtils

try
    using Revise
catch
end

module JuliaClientRuntime

using Sockets

function _file(frame)
    return replace(String(frame.file), '\\' => '/')
end

function _module_name(frame)
    mod = parentmodule(frame)
    return mod === nothing ? "Unknown" : string(mod)
end

function _roots()
    roots = String[pwd()]
    try
        active = Base.active_project()
        active === nothing || push!(roots, dirname(active))
    catch
    end
    try
        push!(roots, joinpath(homedir(), ".julia", "dev"))
    catch
    end
    return unique!(replace.(abspath.(roots), '\\' => '/'))
end

function _under(path, root)
    root == "" && return false
    return path == root || startswith(path, root * "/")
end

function _is_julia_internal(path)
    startswith(path, "./") && return true
    contains(path, "/base/") && return true
    contains(path, "/julia/stdlib/") && return true
    contains(path, "/share/julia/stdlib/") && return true
    return false
end

function _is_pkg_cache(path)
    return contains(path, "/.julia/packages/")
end

function _debug_entries()
    entries = split(get(ENV, "JULIA_DEBUG", ""), ",")
    return strip.(filter(x -> !isempty(x), entries))
end

function _user_frame(frame, roots, debug_entries)
    path = _file(frame)
    pseudo_path = startswith(path, "./") ? path[3:end] : path
    pseudo_path == "julia-client-eval" && return true
    startswith(path, "REPL") && return true

    base = splitext(basename(path))[1]
    mod = _module_name(frame)
    for entry in debug_entries
        startswith(entry, "!") && continue
        if entry == base || entry == mod
            return true
        end
    end

    if !(startswith(path, "/"))
        return false
    end
    apath = replace(abspath(path), '\\' => '/')
    any(root -> _under(apath, root), roots) && return true
    contains(apath, "/.julia/dev/") && return true
    _is_julia_internal(apath) && return false
    _is_pkg_cache(apath) && return false
    return false
end

function _visible_indices(frames)
    roots = _roots()
    debug_entries = _debug_entries()
    user = findall(frame -> _user_frame(frame, roots, debug_entries), frames)
    visible = Set{Int}()
    for i in user
        push!(visible, i)
        i > 1 && push!(visible, i - 1)
    end
    isempty(visible) && !isempty(frames) && push!(visible, 1)
    return sort!(collect(visible))
end

function _omitted_modules(frames, first_i, last_i)
    first_i > last_i && return ""
    modules = String[]
    for frame in @view frames[first_i:last_i]
        name = _module_name(frame)
        name in ("Core", "Main.Unknown") && continue
        push!(modules, name)
    end
    unique!(modules)
    length(modules) > 6 && (modules = vcat(modules[1:6], ["..."]))
    isempty(modules) && push!(modules, "Unknown")
    return "      ... internal @ " * join(modules, ", ")
end

function _frame_line(i, frame)
    sig = split(sprint(show, frame), " at "; limit=2)[1]
    file = String(frame.file)
    line = frame.line == 0 ? "?" : string(frame.line)
    return "  [" * string(i) * "] " * sig * " @ " * file * ":" * line
end

function _render_selected(frames; include_omitted)
    io = IOBuffer()
    visible = _visible_indices(frames)
    isempty(frames) && return ""
    println(io, "Stacktrace:")
    last_i = 0
    n = 0
    for i in visible
        if include_omitted && i > last_i + 1
            println(io, _omitted_modules(frames, last_i + 1, i - 1))
        end
        n += 1
        println(io, _frame_line(n, frames[i]))
        last_i = i
    end
    if include_omitted && last_i < length(frames)
        println(io, _omitted_modules(frames, last_i + 1, length(frames)))
    end
    return String(take!(io))
end

function _display_error(err)
    while err isa LoadError
        err = err.error
    end
    return err
end

function _is_test_summary_exception(err)
    err = _display_error(err)
    name = nameof(typeof(err))
    mod = parentmodule(typeof(err))
    return string(nameof(mod)) == "Test" && string(name) in ("TestSetException", "FallbackTestSetException")
end

function _render_error(err, bt)
    frames = stacktrace(bt)
    cut = findfirst(frame -> frame.func === :include_string || String(frame.file) == "none", frames)
    cut === nothing || (frames = frames[1:max(cut - 2, 1)])
    display_err = _display_error(err)
    test_exception = _is_test_summary_exception(display_err)
    short_body = if test_exception
        sprint() do io
            Base.invokelatest(showerror, io, display_err, bt)
        end
    else
        sprint(showerror, display_err)
    end
    short = "ERROR: " * short_body
    if test_exception
        smart = short
    else
        smart =  short * "\n" * _render_selected(frames; include_omitted=true)
    end
    full = sprint(showerror, err, bt)
    return short, smart, full
end

# Control channel: dial back to the loopback address the Go client passed and
# authenticate with the shared token. One framed status line per eval flows out
# (OK/ERR); interrupt bytes flow back in. A socket rather than an inherited fd
# is what makes this work on Windows.
const _CONTROL = try
    host, port = rsplit(ENV["REPLD_CONTROL_ADDR"], ":"; limit=2)
    sock = Sockets.connect(host, parse(Int, port))
    println(sock, ENV["REPLD_CONTROL_TOKEN"])
    flush(sock)
    sock
catch
    nothing
end

function _write_control(line)
    _CONTROL === nothing && return
    println(_CONTROL, line)
    flush(_CONTROL)
end

_hex(s) = bytes2hex(Vector{UInt8}(codeunits(s)))

# Interrupt channel: the client writes a byte on the same control socket; we
# forward it to the running eval task as a catchable InterruptException. SIGINT
# is unusable here — with stdin piped + multiple threads it lands on the wrong
# task and crashes. The socket read yields through libuv's event loop.
const _CURRENT = Ref{Union{Task,Nothing}}(nothing)

function _start_interrupt_listener()
    _CONTROL === nothing && return
    @async while true
        try
            read(_CONTROL, UInt8)
        catch
            break # EOF: client gone
        end
        t = _CURRENT[]
        isnothing(t) && continue
        try
            schedule(t, InterruptException(); error=true)
        catch
        end
    end
    return nothing
end

function run(hex_code, print_result)
    code = String(hex2bytes(hex_code))
    # Eval on a worker task so the fd-4 listener can target it.
    task = Task() do
        try
            isdefined(Main, :Revise) && Main.Revise.revise()
        catch
        end
        include_string(Main, code, "julia-client-eval")
    end
    _CURRENT[] = task
    schedule(task)
    local value
    err = bt = nothing
    try
        value = fetch(task)
    catch
        err, bt = task.result, task.backtrace
    finally
        _CURRENT[] = nothing
    end
    if isnothing(err)
        if print_result
            show(IOContext(stdout, :limit => true), MIME("text/plain"), value)
            println(stdout)
        end
        flush(stdout)
        _write_control("OK")
    else
        short, smart, full = _render_error(err, bt)
        flush(stdout)
        _write_control(string("ERR ", _hex(short), " ", _hex(smart), " ", _hex(full)))
    end
    return nothing
end

_start_interrupt_listener()

end
