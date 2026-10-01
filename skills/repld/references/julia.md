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

When `Revise` is available, repld loads it and calls `Revise.revise()` before each eval. Tracking depends on load path: dev'd packages pick up method and `const`/global changes. `includet`'d files only patch method unless files/modules set `__revise_mode__ = :eval`.

New dep in a dev'd package: `Pkg.resolve()`, then re-save the file (Revise won't retry a failed revision).

## Traceback levels (`--trace`)

- `short`: exception message only.
- `smart`: default; user/project frames plus nearby boundary frames, hiding Julia/client internals.
- `full`: Julia's full traceback.

```bash
repld --trace full julia -e 'error("boom")'
repld trace --trace smart julia             # show last saved traceback, no rerun
```
