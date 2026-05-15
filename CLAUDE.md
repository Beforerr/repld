## Architecture

Single Go binary (`go/`) that doubles as client and daemon.

**Client mode** (default): walks up `$PWD` for `Project.toml` to pick the env, then sends a JSON request over `~/.local/share/julia-client/julia-daemon.sock`. If the daemon isn't running, re-execs itself as `daemon` in the background first.

**Daemon mode** (`daemon` subcommand): long-lived server (`go/daemon.go`) holding a `SessionManager`. Sessions are keyed by absolute project path (or a temp dir when no project is found). Shuts down after 1 hour of inactivity.

**Session** (`go/session.go`): wraps a single `julia -i --threads=auto --project=<dir>` subprocess. stdout+stderr are merged into one pipe. Code is hex-encoded and eval'd via `include_string(Main, String(hex2bytes("...")))` to avoid quoting issues.

## Key files

- `go/main.go` - CLI flags, env detection, client send/receive
- `go/daemon.go` - Request dispatch, inactivity timer
- `go/session.go` - `JuliaSession` + `SessionManager`
