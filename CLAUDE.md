## Architecture

Full architecture, wire protocol, and key-file map: **[docs/architecture.md](docs/architecture.md)**. Orientation:

Single Go binary `repld` (`go/`), client + daemon, polyglot. CLI is `repld [repld-flags] <exe> [interp-flags] -e/-c CODE`: the interpreter is a leading positional, language resolved from `--lang` > exe basename.

Each eval drives the interpreter subprocess over three channels — stdout/stderr (user output, NDJSON frames), a sentinel (drain barrier), and a loopback TCP control socket (framed `OK`/`ERR` status + interrupt bytes). Before touching `go/session.go` or a runtime, read the wire-protocol invariants in docs/architecture.md — the concurrent control-frame drain and the degrade-don't-hang rules are easy to break.
