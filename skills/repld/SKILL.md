---
name: repld
description: "Evaluate Julia/Python/R/Wolfram in long-lived background sessions so imports, variables, and project state persist across calls. Use for iterative work — package development, REPL-style experiments, tests, benchmarks — where starting fresh each time would be wasteful."
---

## Preferred workflow

Treat `repld <exe>` like a persistent REPL. Run setup once, then reuse loaded imports, variables, fixtures, and project state across later calls for the same session.

```bash
repld julia -E 'using ImportPackageOnce; x = load_fixture();'
repld julia -E 'transform(x)'

repld python3 -c 'import numpy as np; a = np.arange(5)'
repld python3 -c 'print(a.sum())'

repld R -e 'x <- rnorm(100)'
repld R -e 'mean(x)'

repld wolframscript -c 'x = Range[5]'
repld wolframscript -c 'Total[x]'

repld --session scratch julia -E 'x = 1'  # named session across directories
cd /tmp && repld --session scratch julia -E 'x'  # reuse existing named session
```

- Import packages once per session; later calls should use already-loaded names.
- Avoid repeating fixture/setup code in every command.
- Repld flags (`--session`, `--fresh`, `--lang`) go before the interpreter. Interpreter flags go after it.
- Session routing: `--session LABEL` has highest priority. Otherwise sessions are keyed by language plus adapter-specific environment (for example project flag for Julia, interpreter path for Python/R/Wolfram) plus cwd.
- Avoid using `--fresh` when a live interpreter can safely pick up changed state.

See [references/julia.md](references/julia.md) for Julia-specific notes.

## Commands

```bash
repld trace --session scratch               # last saved traceback, no rerun
repld interrupt --session scratch           # interrupt stuck eval; state kept on Julia/Python/R, Wolfram = kill
repld sessions                              # list active sessions
repld stop                                  # shut down daemon

timeout 30 repld julia -e 'might_hang()'    # client death interrupts eval
```
