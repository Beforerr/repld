## Architecture

Single Go binary (`go/`) that doubles as client and daemon.

**Client mode** (default): walks up `$PWD` for `Project.toml` to pick the env, then sends a JSON request over `~/.local/share/julia-client/julia-daemon.sock`. If the daemon isn't running, re-execs itself as `daemon` in the background first.

**Daemon mode** (`daemon` subcommand): long-lived server (`go/daemon.go`) holding a `SessionManager`. Sessions are keyed by absolute project path (or a temp dir when no project is found). Shuts down after 1 hour of inactivity.

**Session** (`go/session.go`): wraps a single `julia [+channel] -i <forwarded switches> --project=<dir>` subprocess. Switches julia-client doesn't own are forwarded verbatim, applied only at session creation. Code is hex-encoded and eval'd via `include_string(Main, String(hex2bytes("...")))` to avoid quoting issues.

## Eval wire protocol (session ↔ Julia subprocess)

Three channels:

- **stdout / stderr** — separate OS pipes Go owns directly; user output only, streamed as NDJSON `chunk`/`stderr` frames. Captures C-level/subprocess writes too — do not redirect inside Julia.
- **Sentinel** — `println(sentinel)` to both streams after each eval; pure drain barrier, no data.
- **Control** — a loopback TCP socket. Go listens on `127.0.0.1:0` and passes the address + a one-time token via env (`JULIA_CLIENT_CONTROL_ADDR`/`_TOKEN`); the runtime dials back and authenticates. Bidirectional over one conn: runtime writes one frame per eval (`OK` or `ERR <hex short> <hex smart> <hex full>`, the source of truth for errors — stdout is never parsed for them); Go writes a byte the other way to interrupt. A socket, not inherited fds, because `cmd.ExtraFiles` is unsupported on Windows.

Invariants:
- Drain the control frame **concurrently** (own goroutine), never after the sentinel — a large frame can fill the socket buffer and deadlock otherwise.
- No connection (token mismatch / dial failed) → degrade to no error/no interrupt, not hang. `executeRaw` reads the frame only when `expectControl` is set; EOF yields `nil`.
- `start()` blocks until the control conn is accepted (or times out) before the first eval; startup calls pass `expectControl=false` (no `run()`, no frame).

## Key files

- `go/main.go` - CLI arg scanner (`parseArgs`: own flags vs forwarded switches), client send/receive
- `go/daemon.go` - Request dispatch, inactivity timer, client-disconnect→interrupt watcher
- `go/session.go` - `JuliaSession`: subprocess lifecycle, `executeRaw` (sentinel + control protocol), `execute`, interrupt
- `go/manager.go` - `SessionManager`: session map, routing/keying, per-session logs
- `go/julia_client_runtime.jl` - in-Julia runtime: dials the control socket, `run()` evals code and writes the control frame, listens for interrupt bytes; error/traceback rendering
