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

# File mode: write/update scripts, then eval them in the warm session
repld julia analysis.jl
repld python3 train.py 50  # 50 → sys.argv[1]
```

- Import packages once per session; later calls should use already-loaded names.
- Avoid repeating fixture/setup code in every command.
- Repld flags (`--session`, `--fresh`, `--lang`) go before the interpreter. Interpreter flags go after it.
- Session routing: `--session LABEL` has highest priority. Otherwise sessions are keyed by language plus adapter-specific environment (for example project flag for Julia, interpreter path for Python/R/Wolfram) plus cwd.
- Avoid using `--fresh` when a live interpreter can safely pick up changed state.

See [julia.md](references/julia.md), [python.md](references/python.md), [r.md](references/r.md), [wolfram.md](references/wolfram.md) for language-specific notes.

## Commands

```bash
repld sessions                              # list active sessions, show IDs
repld trace <id | --session=LABEL>          # last saved traceback
repld interrupt <id | --session=LABEL>
repld close <id | --session=LABEL>
repld stop                                  # shut down daemon

timeout 30 repld julia -e 'might_hang()'    # client death interrupts eval
```
