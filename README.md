# julia-client

Persistent Julia REPL client and daemon.

Runs Julia code in long-lived sessions over a Unix socket so that state (variables, loaded packages) survives between calls. The project is auto-detected from `$PWD` by default; see [Sessions](#sessions) for routing.

## Quickstart

```bash
curl -fsSL https://raw.githubusercontent.com/Beforerr/julia-client/main/install.sh | bash
# Override destination with `INSTALL_DIR=/usr/local/bin`.
```

This installs `julia-client` to `~/.local/bin`. The single binary acts as both client and daemon (daemon auto-starts on first `eval`).

To uninstall: `rm "$(which julia-client)"`.

## Agent skill

The included skill at `skills/julia-client/SKILL.md` teaches Agent how to use `julia-client`.

```bash
npx skills add https://github.com/Beforerr/julia-client
```

Or manually by adding this repo's `skills/` directory to your Agent skill search paths.

## Usage

```bash
# Evaluate code (daemon starts automatically)
julia-client -e 'println("hello")'

# Use a custom Julia binary via JULIA_EXE environment variable
JULIA_EXE=/path/to/julia julia-client -e 'println("hello")'

# Explicit project: each distinct --project is its own session
julia-client --project /path/to/project -e 'using MyPackage'
julia-client --project @temp -e 'using Pkg; Pkg.add("Example")'
julia-client --project @myenv -e 'using Example'

# Read from stdin
echo 'println("hello")' | julia-client

# Session management
julia-client sessions   # list active sessions
julia-client trace      # show the last saved Julia traceback without rerunning
julia-client stop       # shut down the daemon

# Traceback levels
julia-client --trace full -e 'error("boom")'
julia-client trace --trace smart
```

Traceback levels:

- `short`: exception message only.
- `smart`: default eval output; user/project frames plus nearby boundary frames, hiding Julia/client internals.
- `full`: Julia's full traceback, including runtime and REPL frames.

## Sessions

Each call is routed to a persistent Julia process by a key, in priority order:

1. `--session LABEL` — explicit label, shared across directories and projects.
2. `--project PROJECT` (not `@.`) — the selector (`@temp`, `@myenv`) or absolute path.
3. default (`@.` or omitted) — the current directory; Julia uses the nearest `Project.toml` up from `$PWD`.

A session's project is fixed at launch: a different `--project` routes to (and starts) a *different* session, not a reactivation. Same key reuses the same process, so its state persists; different keys are isolated. `--fresh` restarts the targeted session.

## Architecture

A single `julia-client` binary serves as both client and daemon:

- **Client mode** (default) — sends JSON requests over a Unix socket (`~/.local/share/julia-client/julia-daemon.sock`)
- **Daemon mode** (`julia-client daemon`) — background server managing persistent Julia processes; auto-started on first `eval`, shuts down after 1 hour of inactivity

## Alternatives

- [julia-mcp](https://github.com/aplavin/julia-mcp?tab=readme-ov-file) is very similar but uses MCP server instead
- [DaemonicCabal.jl](https://github.com/tecosaur/DaemonicCabal.jl) only runs on Linux
