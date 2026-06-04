# Julia (`repld julia`)

Persistent Julia REPL. `julia` below is the interpreter (or an absolute path / `+channel` form). The project is auto-detected from `$PWD` (nearest `Project.toml`) unless overridden.

## Usage

```bash
repld julia -e 'println("hello")'           # eval (daemon auto-starts)
repld julia -E '1 + 1'                       # eval and display the result

repld julia script.jl                        # run a file

# Explicit project: each distinct --project is its own session
repld julia --project /path/to/project -e 'using MyPackage'
repld julia --project @temp -e 'using Pkg; Pkg.add("Example")'

repld --session scratch julia -e 'x = 1'     # named session, shared across dirs
repld julia -t 4 -e 'Threads.nthreads()'     # threads for a new session
repld julia +1.11 -e 'VERSION'               # juliaup channel (forwarded to julia)
repld /opt/julia-1.10/bin/julia -e 'VERSION' # a specific binary
```

Any switch repld doesn't own is forwarded verbatim to the `julia` subprocess at session creation (e.g. `--startup-file=no`, `-L init.jl`). After editing package code, `Revise` updates definitions automatically; use `--fresh` when it can't (struct/type redefinition, new `using` of a dep not yet in the module's `Project.toml`).

## Traceback levels (`--trace`)

- `short`: exception message only.
- `smart`: default; user/project frames plus nearby boundary frames, hiding Julia/client internals.
- `full`: Julia's full traceback.

```bash
repld --trace full julia -e 'error("boom")'
repld trace --trace smart julia             # show last saved traceback, no rerun
```

## Sessions

Routing key, in priority order:

1. `--session LABEL` — explicit label, shared across directories and projects.
2. `--project PROJECT` (not `@.`) — selector (`@temp`, `@myenv`) or absolute path.
3. default (`@.`) — current directory; Julia uses the nearest `Project.toml`.

A session's project is fixed at launch; a different `--project` routes to a different session.
