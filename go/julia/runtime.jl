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
    pseudo_path == "repld-eval" && return true
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
        sprint(io -> Base.invokelatest(showerror, io, display_err))
    end
    short = "ERROR: " * short_body
    if test_exception
        smart = short
    else
        smart =  short * "\n" * _render_selected(frames; include_omitted=true)
    end
    full = sprint(io -> Base.invokelatest(showerror, io, err, bt))
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

# FS events reach Revise's watcher tasks a few hundred ms after a save, so an eval
# sent right after an edit would run stale code. Sweep tracked files' ctimes now and
# queue changes as `Revise.revise_dir_queued` does. Revise internals: on mismatch this
# throws and we fall back to the async watcher alone.
function _queue_unseen_edits(R)
    for (dir, wl) in (@lock R.revise_lock collect(R.watched_files))
        tracked = @lock R.revise_lock collect(wl.trackedfiles)
        for (file, id) in R.scan_changed_files(dir, wl, tracked, nothing)
            id == R.NOPACKAGE && continue
            path = joinpath(dir, file)
            @lock R.revise_lock begin
                pkgdata = R.pkgdatas[id]
                R.hasfile(pkgdata, path) && push!(R.revision_queue, (pkgdata, relpath(path, pkgdata)))
            end
        end
    end
end

function _revise()
    isdefined(Main, :Revise) || return
    R = Main.Revise
    try
        Base.invokelatest(_queue_unseen_edits, R)
    catch
    end
    try
        R.revise()
    catch
    end
end

# Package changes a live session can't apply, while Revise handles the rest of
# Project.toml/Manifest.toml (versions, new deps, dev paths). Base registers a
# package's extensions only when it loads; Revise with `extensions_stale` re-registers
# added ones, but changed or removed ones never take effect. A manifest change can move
# a registered (non-dev'd) package to another version. Both checks are mtime-gated:
# they parse or re-resolve.
const _PROJECTS = Dict{String,Tuple{Float64,Any}}()  # dev'd Project.toml => (mtime, [extensions] at load)
const _MANIFEST_MTIME = Ref{Pair{String,Float64}}("" => 0.0)

_is_mutable_pkg(path) = !startswith(path, Sys.STDLIB) &&
    !any(d -> startswith(path, joinpath(d, "packages")), DEPOT_PATH)

_extensions(proj) = get(Base.parsed_toml(proj), "extensions", nothing)

function _on_load(id::Base.PkgId)
    try
        dir = pkgdir(Base.root_module(id))
        (dir === nothing || !_is_mutable_pkg(dir)) && return
        proj = joinpath(dir, "Project.toml")
        isfile(proj) && !haskey(_PROJECTS, proj) && (_PROJECTS[proj] = (mtime(proj), _extensions(proj)))
    catch
    end
    return
end

function _record_manifest()
    proj = Base.active_project()
    manifest = proj === nothing ? nothing : Base.project_file_manifest_path(proj)
    _MANIFEST_MTIME[] = manifest === nothing ? ("" => 0.0) : (manifest => mtime(manifest))
end

_only_added(old, new) = old === nothing || (new !== nothing && all(((k, v),) -> get(new, k, nothing) == v, old))

function _env_changes()
    reasons = String[]
    for (proj, (t, exts)) in _PROJECTS
        (!isfile(proj) || mtime(proj) == t) && continue
        new = _extensions(proj)
        if new == exts
            _PROJECTS[proj] = (mtime(proj), exts)
        elseif _only_added(exts, new) && isdefined(Main, :Revise) && isdefined(Main.Revise, :extensions_stale)
            # Revise reacts only to manifest changes, but Base reads an active project's
            # own [extensions] from Project.toml, as a fresh interpreter would.
            Main.Revise.extensions_stale[] = true
            _PROJECTS[proj] = (mtime(proj), new)
        else
            push!(reasons, "[extensions] in " * proj * " changed after its package loaded")
        end
    end
    manifest, t = _MANIFEST_MTIME[]
    if !isempty(manifest) && mtime(manifest) != t
        moved = String[]
        for (id, mod) in Base.loaded_modules
            path, now = pathof(mod), Base.locate_package(id)
            (path === nothing || now === nothing || path == now || _is_mutable_pkg(path)) || push!(moved, id.name)
        end
        isempty(moved) ? _record_manifest() :
            push!(reasons, manifest * " moved loaded packages: " * join(sort!(moved), ", "))
    end
    return reasons
end

# One reason per line on stdout; the daemon restarts a file-mode session that has
# any, and warns in eval mode.
function stale_reasons()
    reasons = _env_changes()
    if isdefined(Main, :Revise)
        # Revise logs a failed revision once, then keeps running the old code.
        failed = sort!(unique!([file for (_, file) in keys(Main.Revise.queue_errors)]))
        isempty(failed) || push!(reasons, "Revise could not apply edits to " * join(failed, ", ") *
            "; their old code is still live (`Revise.errors()` for details)")
    end
    foreach(println, reasons)
end

function run(hex_code, print_result)
    code = String(hex2bytes(hex_code))
    # Eval on a worker task so the fd-4 listener can target it.
    task = Task() do
        _revise()
        include_string(Main, code, "repld-eval")
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
            # invokelatest: eval may load a package, bumping world age past this frame;
            # without it, showing a value whose method was just defined (e.g. a new
            # @enum's namemap) throws "method too new".
            Base.invokelatest(show, IOContext(stdout, :limit => true), MIME("text/plain"), value)
            println(stdout)
        end
        flush(stdout)
        _write_control("OK")
    else
        short, smart, full = Base.invokelatest(_render_error, err, bt)
        flush(stdout)
        _write_control(string("ERR ", _hex(short), " ", _hex(smart), " ", _hex(full)))
    end
    return nothing
end

_start_interrupt_listener()
_record_manifest()
push!(Base.package_callbacks, _on_load)

end
