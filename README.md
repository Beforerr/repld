# julia-client

Persistent Julia REPL client and daemon.

Runs Julia code in a long-lived session over a Unix socket so that state (variables, loaded packages) survives between calls. The project environment is auto-detected from `$PWD`.

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

# Explicit project environment
julia-client --project /path/to/project -e 'using MyPackage'
julia-client --project @temp -e 'using Pkg; Pkg.add("Example")'
julia-client --project @shareAnyname -e 'using Example'

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

## Architecture

A single `julia-client` binary serves as both client and daemon:

- **Client mode** (default) — sends JSON requests over a Unix socket (`~/.local/share/julia-client/julia-daemon.sock`)
- **Daemon mode** (`julia-client daemon`) — background server managing persistent Julia processes; auto-started on first `eval`, shuts down after 1 hour of inactivity

## Alternatives

- [julia-mcp](https://github.com/aplavin/julia-mcp?tab=readme-ov-file) is very similar but uses MCP server instead
- [DaemonicCabal.jl](https://github.com/tecosaur/DaemonicCabal.jl) only runs on Linux
