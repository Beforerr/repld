---
name: julia-client
description: "Run Julia code in persistent sessions: setup once, then reuse variables, imports, and project state across commands. Use for efficient Julia execution, testing, and development."
---

## Examples

```bash
julia-client -e 'using MyPackage; x = load_fixture()' # Setup once; omit default --project=. for cwd
julia-client -E 'MyPackage.transform(x)'              # Display result; reuses same session
# Edit MyPackage.transform, then run again; Revise updates definitions in background
julia-client -E 'MyPackage.transform(x)'

julia-client --project=test -e 'using ImportPackageOnce' # Different --project = different session with independent state
julia-client --session scratch -e 'error("test error")' # separate named session

# Long-running tasks (first plot, heavy compute): set longer timeout or disable timeout (0)
julia-client --timeout 300 heavy_script.jl

julia-client trace --session scratch # show the last saved Julia traceback without rerunning
```

## Tips

- Treat sessions like REPL, rely on `Revise` for automatic updates, avoid repeating setup. Only use `--fresh` flag when clean state is required.
- Prefer stacking environments to share dependencies and state across projects, avoiding duplicate setup: `empty!(LOAD_PATH); append!(LOAD_PATH, ["@", "test", "docs", "@v#.#", "@stdlib"])`.

## Session management

```bash
julia-client sessions   # list active sessions
julia-client stop       # shut down the daemon
```
