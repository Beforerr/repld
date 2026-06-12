# repld

Stateful and persistent REPL-like kernel for fast incremental iteration (Act → observe → adjust). CLI/agent-first design.

## Quickstart

```bash
curl -fsSL https://raw.githubusercontent.com/Beforerr/repld/main/install.sh | bash
# Installs to ~/.local/bin. Override with `INSTALL_DIR` env var.
# To uninstall: `repld stop && rm "$(which repld)"`.
```

Code is executed in long-lived sessions so state (variables, loaded packages/modules) persist between calls.

```bash
# repld <exe> [interp-args] (--<eval> CODE | <file> | -)
repld julia -e 'using LinearAlgebra; A = rand(3,3)'
repld julia -E 'det(A)'                 # reuses warm session

repld python3 -c 'import numpy as np; a = np.arange(5)'
repld python3 -c 'print(a.sum())'       # reuses loaded numpy + a
repld .venv/bin/python -c '...'         # point at interpreter directly

repld R -e 'library(stats); x <- rnorm(100)'
repld R -e 'mean(x)'                    # reuses warm R session

repld wolframscript -c 'x = Range[5]'
repld wolframscript -c 'Total[x]'
```

Eval flags use each language's native spelling — Julia: `-e` / `-E`, Python: `-c`, R: `-e`, WolframScript: `-c`.

Per-language docs: [julia](skills/repld/references/julia.md) · [python](skills/repld/references/python.md) · [R](skills/repld/references/r.md) · [Wolfram](skills/repld/references/wolfram.md).


## Agent skill

The skill at `skills/repld/SKILL.md` teaches agents how to use repld.

```bash
npx skills add https://github.com/Beforerr/repld
```

## Usage

```bash
repld julia/python3/R/wolframscript FILE [args...]

repld --session LABEL <exe> ...   # named session, reusable across dirs
repld --fresh <exe> ...           # restart targeted session first
repld --trace LEVEL <exe> ...     # error traceback level: short | smart | full

repld trace [--session=L | exe]   # last saved traceback
repld interrupt [--session=L | exe] # interrupt in-flight eval
repld sessions                    # list active sessions
repld stop                        # shutdown daemon
```

`<exe>` is `julia`, `python3`, `R`, `wolframscript`, an absolute/relative interpreter path, etc. repld's own flags (`--socket`/`--session`/`--lang`/`--trace`/`--fresh`) go before `<exe>`; after it, every flag forwards verbatim to the interpreter (e.g. Julia's `--project=DIR`, `+1.11` for juliaup) except native eval/print flags (`-e`/`-c`/`-E`). Each call routes to a persistent session keyed by language + project + `--session`/cwd.

## Architecture

One Go binary is both the CLI client and the background daemon (auto-started on first use, stopped with `repld stop`). For design details, wire protocol, and key-file map, see [docs/architecture.md](docs/architecture.md).

## Alternatives

- [Jupyter](https://jupyter.org) (IJulia, ipykernel) is the established polyglot, heavier version of this idea: persistent kernels over ZeroMQ with rich display and browser frontend, kernels for ~100 languages. However
    - Agent needs none of the UI layer. Rich display protocol (interactive widgets, HTML reprs, plot embedding) is nice for humans but irrelevant for headless agents.
    - Notebook `.ipynb` format is noisy. JSON wrapper with cell metadata, output mime-bundles, execution counts. Agent thinks better with plain code + stdout.
    - This tool is the minimal slice: one dependency-free binary, a plain text protocol — trading rich MIME output for simplicity and zero setup.
- Model Context Protocol (MCP) Server is less composable than shell tools and infeasible to use from outside AI sessions.
    - [mcp-repl](https://github.com/posit-dev/mcp-repl) for persistent Python/R with sandboxing, inline plot images, curated oversized output.
    - [julia-mcp](https://github.com/aplavin/julia-mcp?tab=readme-ov-file) for Julia
- Julia
    - [DaemonicCabal.jl](https://github.com/tecosaur/DaemonicCabal.jl) only runs on Linux
    - [Malt.jl](https://github.com/JuliaPluto/Malt.jl) manages isolated Julia worker processes *from within Julia* (used by Pluto). Both run code in persistent, crash-isolated subprocesses, but Malt is a Julia library: its driver must be Julia, and it returns native typed values over Julia's serialization. This tool targets non-Julia callers — a single dependency-free binary speaking a text protocol, so any language/shell/agent can drive it and interpreter versions can be mixed freely.
