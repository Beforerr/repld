# `repld julia`

## Session routing

Without specifying `--session`, distinct projects correspond to different sessions.

```bash
repld julia --project=test -e 'using ImportPackageOnce'
repld julia --project=@temp -e 'using Pkg; Pkg.add("Example")'
repld julia +1.11 -E 'VERSION' # default project env is "@."
repld --fresh julia -t 4 -E 'Threads.nthreads()'
```

## Revise

When `Revise` is available, repld loads it and calls `Revise.revise()` before each eval. Tracking depends on load path: dev'd packages pick up method and `const`/global changes. `includet`'d files only patch method unless files/modules set `__revise_mode__ = :eval`.

Use `--fresh` for untrackable changes, such as:

- Struct/type redefinition.
- `using NewPkg` inside modules whose `Project.toml` did not list `NewPkg` when session was created.

## Traceback levels (`--trace`)

- `short`: exception message only.
- `smart`: default; user/project frames plus nearby boundary frames, hiding Julia/client internals.
- `full`: Julia's full traceback.

```bash
repld --trace full julia -e 'error("boom")'
repld trace --trace smart julia             # show last saved traceback, no rerun
```
