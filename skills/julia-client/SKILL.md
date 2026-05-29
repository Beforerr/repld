---
name: julia-client
description: "Evaluate Julia in long-lived background sessions so imports, variables, and project state persist across calls. Use for iterative Julia work — package development, REPL-style experiments, tests, benchmarks — where starting fresh each time would be wasteful."
---

## Preferred workflow

Treat `julia-client` like persistent REPL: run setup once with `julia-client -E 'using MyPackage; x = load_fixture()'`, then `julia-client -E 'MyPackage.transform(x)'` to display results while reusing session. After editing package code, `Revise` automatically updates definitions.

- Don't repeat yourself
- Import packages once per session; later calls should use already-loaded names.
- Avoid repeating fixture/setup code in every command.
- Session routing: `--session LABEL` > `--project PROJECT`; default to `--project=@.` when omit both.
- Use `--fresh` only when Revise can't track changes. Some cases include:
  - struct/type redefinition
  - adding `using NewPkg` inside modules whose `Project.toml` didn't already list `NewPkg` dep (the module's load-time dep view is cached so `Pkg.add`+edit isn't enough)

## Environment

- `JULIA_EXE` — Path to the Julia binary. If unset, `julia` is looked up in `$PATH`.
  Example: `JULIA_EXE=/opt/julia/bin/julia julia-client -e 'using MyPackage'`

## Other Examples

```bash
julia-client --project=test -e 'using ImportPackageOnce' # Different --project = different session with independent state
julia-client --project=@temp -e 'using Pkg; Pkg.add("Example")' # Temporary throwaway environment
julia-client --session scratch -e 'error("test error")' # separate named session

julia-client trace --session scratch # show the last saved Julia traceback without rerunning

julia-client sessions   # list active sessions
julia-client interrupt --session scratch  # SIGINT a stuck call; session state preserved if it responds within 3s
timeout 30 julia-client -e 'might_hang()'  # killing the client interrupts the eval too — no orphaned computation
julia-client stop       # shut down the daemon
```
