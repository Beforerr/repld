---
name: julia-client
description: "Evaluate Julia in long-lived background sessions so imports, variables, and project state persist across calls. Use for iterative Julia work — package development, REPL-style experiments, tests, benchmarks — where starting fresh each time would be wasteful."
---

## Preferred workflow

Treat `julia-client` like persistent REPL: run setup once with `julia-client -E 'using MyPackage; x = load_fixture()'`, then `julia-client -E 'MyPackage.transform(x)'` to display results while reusing session. After editing package code, `Revise` automatically updates definitions. Do not repeat imports, fixture setup, or `--project=.` for the current directory.

- Don't repeat yourself: avoid repeating same setup in every command.
- Use `--fresh` flag for struct revision (Revise disables by default) or when clean state is required.

## Other Examples

```bash
julia-client --project=test -e 'using ImportPackageOnce' # Different --project = different session with independent state
julia-client --project=@temp -e 'using Pkg; Pkg.add("Example")' # Temporary throwaway environment
julia-client --session scratch -e 'error("test error")' # separate named session

# Long-running tasks (first plot, heavy compute): set longer timeout or disable timeout (0)
julia-client --timeout 300 heavy_script.jl

julia-client trace --session scratch # show the last saved Julia traceback without rerunning

julia-client sessions   # list active sessions
julia-client stop       # shut down the daemon
```
