# Wolfram (`repld wolframscript`)

Persistent Wolfram Language session driven through `wolframscript`.

```bash
repld wolframscript -c 'x = Range[5]'
repld wolframscript -c 'Total[x]'
```

`-c` is WolframScript's native code flag. Results print like `wolframscript -code`.

The adapter launches `wolframscript -script` with an internal runtime loop so state persists across calls without prompt parsing. CI skips this test unless `wolframscript` is installed; Wolfram licensing is not installed by default on GitHub-hosted runners.
