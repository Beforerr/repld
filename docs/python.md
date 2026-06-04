# Python (`repld <python>`)

Persistent Python REPL: runs `python -u -q -i` per session.

Python starts fast, so the win isn't startup latency — it's **persistent state across calls**: heavy imports (numpy, torch), loaded models, and accumulated variables survive between invocations.

`<exe>` below is the Python interpreter: `python3`, a venv's `.venv/bin/python`, or `"$(uv python find)"`.

## Usage

```bash
repld python3 -c 'print(21 * 2)'         # eval (-c is Python's native eval flag)
repld python3 -c 'print(2 + 2)'           # eval and print the result

repld .venv/bin/python -c 'import numpy as np; a = np.arange(5)'   # use a venv
repld .venv/bin/python -c 'print(a.sum())' # reuses numpy + a

repld --session scratch python3 -c 'x = 1'   # named session, shared across dirs
repld sessions                            # list active sessions (all languages)
repld python3 trace                       # last saved traceback
repld --session scratch python3 interrupt # KeyboardInterrupt the in-flight eval
repld stop                                # stop the daemon
```

Pick environment by pointing at its interpreter. Typical loop: `uv sync` then `repld .venv/bin/python ...`.
