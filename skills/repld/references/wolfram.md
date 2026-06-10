# Wolfram (`repld wolframscript`)

Persistent Wolfram Language session driven through `wolframscript`.

```bash
repld wolframscript -c 'x = Range[5];'  # prints Null, like native -code
repld wolframscript -c 'Total[x]'       # prints 15
```

`-c` matches native `wolframscript -code`: the result always prints (`Null` when end with `;`), Wolfram messages (e.g. `Power::infy` from `1/0`) stream to stderr and do not count as failures — only a syntax error, `Abort[]`, or an uncaught `Throw` reports an error.

The adapter launches `wolframscript -script` with an internal runtime loop so state persists across calls without prompt parsing. CI skips this test unless `wolframscript` is installed; Wolfram licensing is not installed by default on GitHub-hosted runners.
