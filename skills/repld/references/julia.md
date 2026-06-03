# `repld julia`

## Session routing

Without specifying `--session`, distinct projects correspond to different sessions.

```bash
repld julia --project=test -e 'using ImportPackageOnce'
repld julia --project=@temp -e 'using Pkg; Pkg.add("Example")'
repld julia +1.11 -e 'VERSION' # default project env is "@."
```

## Revise

When `Revise` is available, repld loads it at runtime startup and calls `Revise.revise()` before each eval. After editing package code, expect definitions to update in the warm session.
Use `--fresh` for untrackable changes, such as:
- Struct/type redefinition.
- Adding `using NewPkg` inside modules whose `Project.toml` did not already list `NewPkg`.

