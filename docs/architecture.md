# Architecture

[![codecov](https://codecov.io/gh/Beforerr/repld/branch/main/graph/badge.svg)](https://codecov.io/gh/Beforerr/repld)

Single Go binary `repld` (`go/`), acting as both client and daemon, **polyglot**.
This is the one source of truth for how the engine works; README and CLAUDE.md
point here.

## CLI shape

`repld [repld-flags] <exe> [interp-flags] (-e/-c CODE | <file> [script-args...])`

The interpreter is an explicit leading positional. The language is resolved
(`go/config.go: resolveLang`) from `--lang` > exe basename. repld's own flags
(`--socket`/`--session`/`--lang`/`--trace`/`--fresh`) are recognized only
*before* `<exe>`; anything after forwards verbatim to the interpreter, except the
eval flag (`-e`/`-c`/`-E`), which repld captures.

**File mode** (mirrors `<exe> [switches] [--] programfile args...`): with no eval flag captured, the first non-flag positional after `<exe>` naming an existing regular file (missing → launch arg) evals in-session; the rest of argv is its script args. `--` forces
the split when a space-form flag value names a file (`-L setup.jl`). Sent
abs-ified as `protocolRequest.File`/`FileArgs`, never read client-side and
never a launch arg: the daemon wraps it via `Adapter.EvalFileStmt` through the
normal eval path, so it re-evals in the warm session on every call.

**Stale sessions.** A file run promises what a freshly launched interpreter would
print. When an existing session can't deliver that — explicit startup args differ
from the ones it was launched with, or the adapter's runtime reports reasons via the
optional `staleChecker` (`go/adapter.go`) — the daemon restarts it first and names
the reasons on stderr. An eval (`-e`/`-c`) keeps the session's state and only warns.

## Modes

**Client mode** (default): sends a `protocolRequest` over the single socket
`~/.local/share/repld/daemon.sock`; re-execs itself as `daemon` if none is
running. Exe (`absExe`) and env come from the client; a new session launches
with them, never the daemon's, which belong to whichever shell first started it.

**Daemon mode** (`daemon` subcommand): one server (`go/daemon.go`) for all
languages; each request's `lang` picks the session's `Adapter`
(`SessionManager.getOrCreate`). Runs until `stop` (or optional
`daemon --idle-timeout SECS`; default 0 = never).

**Session** (`go/session.go`): wraps one interpreter subprocess. The adapter
supplies the launch argv, the embedded runtime source + its load statement, the
per-eval wrapper, and the sentinel statement. Code is hex-encoded and eval'd via
the adapter's wrapper (Julia `include_string(Main, String(hex2bytes("...")))`,
Python `exec(bytes.fromhex("..."))`, R `parse(text = ...)`, Wolfram `ToExpression`) to avoid quoting issues.

## Eval wire protocol (session ↔ interpreter subprocess)

Contract every runtime implements. Three channels:

- **stdout / stderr** — separate OS pipes Go owns directly; user output only,
  streamed as NDJSON `chunk`/`stderr` frames. Captures C-level/subprocess writes
  too — do not redirect inside the interpreter.
- **Sentinel** — the runtime prints the sentinel to both streams after each eval
  (adapter `SentinelStmt`); pure drain barrier, no data.
- **Control** — a loopback TCP socket. Go listens on `127.0.0.1:0` and passes the
  address + a one-time token via env (`REPLD_CONTROL_ADDR`/`_TOKEN`); the runtime
  dials back and authenticates. Bidirectional over one conn: the runtime writes
  one frame per eval (`OK` or `ERR <hex short> <hex smart> <hex full>`, the source
  of truth for errors — stdout is never parsed for them); Go writes a byte the
  other way to interrupt. A socket, not inherited fds, because `cmd.ExtraFiles` is
  unsupported on Windows.

Invariants:
- Drain the control frame **concurrently** (own goroutine), never after the
  sentinel — a large frame can fill the socket buffer and deadlock otherwise.
- No connection (token mismatch / dial failed) → degrade to no error/no
  interrupt, not hang. `executeRaw` reads the frame only when `expectControl` is
  set; EOF yields `nil`.
- `start()` blocks until the control conn is accepted (or times out) before the
  first eval; startup calls pass `expectControl=false` (no `run()`, no frame).

## Interrupt

`Session.interrupt` aborts an in-flight eval, then resends on a slow cadence
(the signal can be silently lost racing the blocked-on wakeup) until the eval
returns or a grace deadline kills the session. The signal path is per-language
(`Adapter.InterruptViaControl`):

- **Julia / Python** — listen for an interrupt byte on the control socket and
  turn it into a catchable in-eval interrupt (InterruptException / KeyboardInterrupt).
  Session survives with state intact.
- **R** — no control listener; the engine sends process `SIGINT`, which R in
  `--slave` non-interactive mode survives.
- **Wolfram** — does not usefully abort a running evaluation on SIGINT; interrupt
  kills the session (state lost).

SIGINT delivery failure degrades to the kill-after-grace path. Interrupting an idle
session is a no-op.

## Key files

- `go/main.go` — CLI arg scanner (`parseArgs`: own flags vs forwarded switches), client send/receive
- `go/config.go` — `langs` registry (name→adapter, eval/print flag spellings), `langForExe`/`resolveLang`, single socket path
- `go/adapter.go` — `Adapter` interface (incl. `EvalFileStmt`); optional `staleChecker`
- `go/daemon.go` — request dispatch, idle watchdog, client-disconnect→interrupt watcher
- `go/session.go` — `Session`: subprocess lifecycle, `executeRaw` (sentinel + control protocol), `execute`, interrupt, graceful `kill`
- `go/manager.go` — `SessionManager`: session map, routing/keying, per-session logs, owner-lease reaper
- `go/owner.go` — owner-pid resolution (client) + liveness/pid-reuse check (daemon)
- `go/julia/`, `go/python/`, `go/r/`, `go/wolfram/` — per-language `Adapter` + embedded runtime

## Adding a language

Implement `Adapter` plus a runtime speaking the wire protocol above; register it
in `langs`. Nothing in the engine changes.
