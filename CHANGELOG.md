# Changelog

## [Unreleased]

- Julia `--project` is now startup-only, like other interpreter flags; later calls reuse cwd session without repeating it.

## [1.0.0]

Reworked into a single language-agnostic binary, `repld`, that selects interpreter from a leading exe positional.

### Breaking

- **Invocation:** `repld <exe> [args] -e/-c CODE`. The interpreter is an
  explicit positional (`repld julia ...`, `repld python3 ...`,
  `repld .venv/bin/python ...`); language inferred from its basename or `--lang`.
- `JULIA_EXE` removed. Pass the interpreter instead: `repld /path/to/julia ...`.
- One daemon, one socket `~/.local/share/repld/daemon.sock` (was per-tool
  `~/.local/share/<tool>/daemon.sock`). Hosts all languages; sessions are keyed
  by language + cwd + environment (Julia's `--project`), so distinct
  languages/projects in one dir are distinct sessions. Existing per-tool daemons
  are orphaned on upgrade.
- **Flag separation:** repld's own flags (`--socket`/`--session`/`--lang`/
  `--trace`/`--fresh`) are recognized only *before* the exe; after it,
  non-eval flags forward to the interpreter (e.g. `repld julia --project=X -e ...`).
- Eval flags are language-native: Julia `-e`/`-E`, Python `-c`.
- `julia-client` binaries removed. Use `repld julia ...`
- `trace` / `interrupt` are verb-first: `repld trace [--session L] [--trace LVL] [exe]`, `repld interrupt [--session L] [exe]`.

### Added

- Python support: `python -u -q -i` sessions, native `-c`, persistent
  namespace, interrupt via `_thread.interrupt_main`, UTF-8 / `\n` output.
