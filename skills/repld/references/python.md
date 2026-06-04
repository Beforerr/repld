# Python (`repld <python>`)

Persistent Python REPL: runs `python -u -q -i` per session.

Pick environment by pointing at its interpreter: `python3`, a venv's `.venv/bin/python`, or `"$(uv python find)"`.

## Usage

```bash
repld python3 -c 'print(2 + 2)'           # eval and print the result

repld .venv/bin/python -c 'import numpy as np; a = np.arange(5)'
repld .venv/bin/python -c 'print(a.sum())'

repld --session scratch python3 -c 'x = 1'
```
