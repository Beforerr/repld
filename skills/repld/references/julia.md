# `repld julia`

## Examples

```bash
repld julia --project=test -e 'using ImportTestPackageOnce'
repld julia -E 'x = ImportTestPackageOnce.result()'
repld --fresh julia --project=@temp -e 'using Pkg; Pkg.add("Example")'
repld --session jl111 julia +1.11 -E 'VERSION'
repld --fresh julia -t 4 -E 'Threads.nthreads()'
```

## Revise

When `Revise` is available, repld loads it and revises before each eval, so saved edits apply on the next call. Dev'd packages pick up method and `const`/global changes; `includet`'d files only patch methods unless files/modules set `__revise_mode__ = :eval`.

Changes a live session can't apply (e.g. a failed Revise edit, changed `[extensions]`) make it stale: a file run restarts it, an eval warns on stderr. A failed edit is retried once its file is re-saved or the manifest changes (e.g. `Pkg.resolve()`), so fix the cause instead of restarting.

## Traceback levels (`--trace`)

- `short`: exception message only.
- `smart`: default; user/project frames plus nearby boundary frames, hiding Julia/client internals.
- `full`: Julia's full traceback.

```bash
repld --trace full julia -e 'error("boom")'
repld trace --trace smart julia             # show last saved traceback, no rerun
```
