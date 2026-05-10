---
name: julia-client
description: "Run Julia code in persistent sessions: setup once, then reuse variables, imports, and project state across commands. Use for efficient Julia execution, testing, and development."
---

## Preferred workflow

Treat `julia-client` like persistent REPL: run setup once with `julia-client -E 'using MyPackage; x = load_fixture()'`, then `julia-client -E 'MyPackage.transform(x)'` to display results while reusing session. After editing package code, run same command again; `Revise` updates definitions automatically. Do not repeat imports, fixture setup, or `--project=.` for the current directory.

## Other Examples

```bash
julia-client --project=test -e 'using ImportPackageOnce' # Different --project = different session with independent state
julia-client --session scratch -e 'error("test error")' # separate named session

# Long-running tasks (first plot, heavy compute): set longer timeout or disable timeout (0)
julia-client --timeout 300 heavy_script.jl

julia-client trace --session scratch # show the last saved Julia traceback without rerunning

julia-client sessions   # list active sessions
julia-client stop       # shut down the daemon
```

## Tips

- Only use `--fresh` flag when clean state is required.
- Prefer stacking environments to share dependencies and state across projects, avoiding duplicate setup: `empty!(LOAD_PATH); append!(LOAD_PATH, ["@", "test", "docs", "@v#.#", "@stdlib"])`.
