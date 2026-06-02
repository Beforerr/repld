// julia-client: CLI for interacting with the julia-daemon persistent REPL.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

var defaultSocket = filepath.Join(os.Getenv("HOME"), ".local", "share", "julia-client", "julia-daemon.sock")

func startDaemon(socketPath string) {
	// Re-exec ourselves with the daemon subcommand — no external dependency.
	self, err := os.Executable()
	if err != nil {
		self = os.Args[0]
	}
	cmd := exec.Command(self, "--socket", socketPath, "daemon")
	cmd.SysProcAttr = sysProcAttrDetach()
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to start daemon: %v\n", err)
	}
}

func connect(socketPath string, startIfNeeded bool) (net.Conn, error) {
	for attempt := range 15 {
		conn, err := net.Dial("unix", socketPath)
		if err == nil {
			return conn, nil
		}
		if !startIfNeeded {
			return nil, fmt.Errorf("julia-daemon is not running (no socket at %s)", socketPath)
		}
		if attempt == 0 {
			startDaemon(socketPath)
		}
		time.Sleep(600 * time.Millisecond)
	}
	return nil, fmt.Errorf("could not connect to julia-daemon at %s after startup — try running 'julia-client daemon' manually to see errors", socketPath)
}

type response struct {
	Output string `json:"output"`
	Error  string `json:"error"`
}

type protocolRequest struct {
	Action      string   `json:"action"`
	Code        string   `json:"code,omitempty"`
	Cwd         string   `json:"cwd,omitempty"`
	Project     string   `json:"project,omitempty"`
	Session     string   `json:"session,omitempty"`
	JuliaArgs   []string `json:"julia_args,omitempty"` // extra switches forwarded to the julia subprocess
	JuliaExe    string   `json:"julia_exe,omitempty"`  // custom Julia binary path
	PrintResult bool     `json:"print_result,omitempty"`
	Fresh       bool     `json:"fresh,omitempty"`
	TraceLevel  string   `json:"trace_level,omitempty"`
}

type streamFrame struct {
	Chunk  string `json:"chunk,omitempty"`
	Stderr string `json:"stderr,omitempty"`
	Done   bool   `json:"done,omitempty"`
	Error  string `json:"error,omitempty"`
}

func run(socketPath string, req protocolRequest, startIfNeeded bool) {
	conn, err := connect(socketPath, startIfNeeded)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	if req.Action == "eval" {
		dec := json.NewDecoder(conn)
		for {
			var f streamFrame
			if err := dec.Decode(&f); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			if f.Done {
				if f.Error != "" {
					fmt.Fprintln(os.Stderr, f.Error)
					os.Exit(1)
				}
				return
			}
			if f.Stderr != "" {
				fmt.Fprint(os.Stderr, f.Stderr)
			}
			if f.Chunk != "" {
				fmt.Print(f.Chunk)
			}
		}
	}

	var resp response
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024)
	if scanner.Scan() {
		if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if resp.Output != "" {
		fmt.Print(resp.Output)
	}
	if resp.Error != "" {
		fmt.Fprintln(os.Stderr, resp.Error)
		os.Exit(1)
	}
}

func mustGetwd() string {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: cannot determine working directory:", err)
		os.Exit(1)
	}
	return cwd
}

func normalizeProjectArg(project string) string {
	if project == "" || strings.HasPrefix(project, "@") {
		return project
	}
	projectArg, _ := filepath.Abs(project)
	return projectArg
}

func cmdEval(socketPath, code, project, session, juliaExe string, printResult, fresh bool, traceLevel string, juliaArgs []string) {
	if code == "-" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		code = string(b)
	}
	req := protocolRequest{
		Action:      "eval",
		Code:        code,
		Cwd:         mustGetwd(),
		Project:     normalizeProjectArg(project),
		Session:     session,
		JuliaExe:    juliaExe,
		TraceLevel:  traceLevel,
		JuliaArgs:   juliaArgs,
		PrintResult: printResult,
		Fresh:       fresh,
	}
	run(socketPath, req, true)
}

func cmdInterrupt(socketPath, project, session string) {
	run(socketPath, protocolRequest{
		Action:  "interrupt",
		Cwd:     mustGetwd(),
		Project: normalizeProjectArg(project),
		Session: session,
	}, false)
}

func cmdTrace(socketPath, project, session, traceLevel string) {
	if traceLevel == "" {
		traceLevel = "full"
	}
	run(socketPath, protocolRequest{
		Action:     "trace",
		Cwd:        mustGetwd(),
		Project:    normalizeProjectArg(project),
		Session:    session,
		TraceLevel: traceLevel,
	}, false)
}

// first returns the first non-empty string.
func first(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func usage() {
	fmt.Fprintf(os.Stderr, `julia-client: Julia REPL client

Usage:
  julia-client [switches] -- [programfile] [args...]
  julia-client [--socket PATH] <command> [options]

Any flag julia-client doesn't recognize is forwarded verbatim to the Julia
subprocess when a session is created, e.g. --startup-file=no, -L init.jl,
+1.11 (juliaup channel, must come first).

Eval flags:
  -e, --eval CODE      Evaluate Julia code (omit or use - to read stdin)
  -E, --print CODE     Evaluate Julia code and display the result
  --project PROJECT    Julia project directory or selector (passed as --project to Julia)
  --session LABEL      Named session to create or reuse across directories
  --fresh              Clear the targeted session before evaluating
  --trace LEVEL        Error traceback level: short, smart, or full (eval default: smart)
  -t, --threads N      JULIA_NUM_THREADS for a newly created session (e.g. 4 or auto)

Session routing (priority order):
  --session LABEL      Shared by label, regardless of directory
  --project PROJECT    Keyed by project path or selector
  (default)            Keyed by current working directory; Julia uses --project=@.

Commands:
  sessions             List active Julia sessions
  trace                Print the last saved Julia error traceback for this session
  interrupt            Interrupt the in-flight eval (SIGKILL after 3s if unresponsive)
  stop                 Stop the daemon
  daemon               Run the daemon in the foreground (normally auto-started)
    --idle-timeout SECS  Shut down after idle (default: 3600)

Global flags:
  --socket PATH        Unix socket path (default: %s)
`, defaultSocket)
	os.Exit(2)
}

var subcommands = map[string]bool{
	"sessions": true, "trace": true, "interrupt": true, "stop": true, "daemon": true,
}

// parsed is the result of scanning the command line: julia-client's own flags,
// the eval mode/code, any leading subcommand, and everything else collected as
// passthrough switches for the Julia subprocess.
type parsed struct {
	socket    string
	juliaExe  string
	project   string
	session   string
	trace     string
	threads   string
	fresh     bool
	evalMode  string // "", "eval", or "print"
	code      string
	files     []string // bare positionals (file candidates)
	juliaArgs []string // forwarded to the subprocess
	sub       string   // leading subcommand, "" if none
	subArgs   []string // tokens after the subcommand
}

// parseArgs consumes julia-client's own flags wherever they appear and forwards
// every other token — unknown flags and their values alike — to juliaArgs. It
// never needs to know Julia's flag arities: a bare token after a forwarded flag
// is forwarded too, so `-L init.jl` survives intact. A bare token before any
// passthrough is a subcommand (only as the first positional) or a file.
func parseArgs(args []string) parsed {
	// Canonical name (dashes/value stripped) -> setter on p.
	value := map[string]func(*parsed, string){
		"e":      func(p *parsed, v string) { p.evalMode, p.code = "eval", v },
		"eval":   func(p *parsed, v string) { p.evalMode, p.code = "eval", v },
		"E":      func(p *parsed, v string) { p.evalMode, p.code = "print", v },
		"print":  func(p *parsed, v string) { p.evalMode, p.code = "print", v },
		"socket": func(p *parsed, v string) { p.socket = v },

		"project": func(p *parsed, v string) { p.project = v },
		"session": func(p *parsed, v string) { p.session = v },
		"trace":   func(p *parsed, v string) { p.trace = v },
		"t":       func(p *parsed, v string) { p.threads = v },
		"threads": func(p *parsed, v string) { p.threads = v },
	}

	p := parsed{socket: defaultSocket, project: "@."}
	for i := 0; i < len(args); {
		t := args[i]
		if strings.HasPrefix(t, "-") {
			name, inline, hasEq := strings.Cut(strings.TrimLeft(t, "-"), "=")
			if set, ok := value[name]; ok {
				if hasEq {
					set(&p, inline)
					i++
				} else if i+1 < len(args) {
					set(&p, args[i+1])
					i += 2
				} else {
					fmt.Fprintf(os.Stderr, "missing value for %s\n", t)
					usage()
				}
				continue
			}
			if name == "fresh" {
				p.fresh = true
				i++
				continue
			}
			p.juliaArgs = append(p.juliaArgs, t) // unknown flag -> forward
			i++
			continue
		}
		// Non-flag token. A leading subcommand wins only before anything else.
		if t[0] != '+' && p.sub == "" && p.evalMode == "" && len(p.juliaArgs) == 0 && len(p.files) == 0 && subcommands[t] {
			p.sub, p.subArgs = t, args[i+1:]
			break
		}
		// A juliaup channel, or a value for a preceding forwarded flag, forwards;
		// an otherwise-standalone bare token is a file candidate.
		if t[0] == '+' || len(p.juliaArgs) > 0 {
			p.juliaArgs = append(p.juliaArgs, t)
		} else {
			p.files = append(p.files, t)
		}
		i++
	}
	return p
}

func main() {
	p := parseArgs(os.Args[1:])

	if p.sub != "" {
		dispatchSubcommand(p)
		return
	}

	// Normalize the thread count into a forwarded -t switch: explicit flag beats
	// an inherited JULIA_NUM_THREADS. Forwarding (rather than inheriting the env)
	// is what makes it effective per-session over the daemon's frozen environment.
	juliaArgs := p.juliaArgs
	if t := first(p.threads, os.Getenv("JULIA_NUM_THREADS")); t != "" {
		juliaArgs = append(juliaArgs, "-t", t)
	}

	// JULIA_EXE selects the Julia binary; empty means look up "julia" in PATH.
	juliaExe := os.Getenv("JULIA_EXE")

	switch {
	case p.evalMode != "":
		cmdEval(p.socket, p.code, p.project, p.session, juliaExe, p.evalMode == "print", p.fresh, p.trace, juliaArgs)
	case len(p.files) > 0:
		f := p.files[0]
		if _, err := os.Stat(f); err != nil {
			fmt.Fprintf(os.Stderr, "unknown command: %s\n", f)
			usage()
		}
		code := fmt.Sprintf("cd(%q) do; Base.include(Main, %q); end", mustGetwd(), f)
		cmdEval(p.socket, code, p.project, p.session, juliaExe, false, p.fresh, p.trace, juliaArgs)
	default:
		// No code given: read stdin only if it's a pipe/redirect, not a terminal.
		fi, err := os.Stdin.Stat()
		if err != nil || fi.Mode()&os.ModeCharDevice != 0 {
			usage()
		}
		cmdEval(p.socket, "-", p.project, p.session, juliaExe, false, p.fresh, p.trace, juliaArgs)
	}
}

func dispatchSubcommand(p parsed) {
	switch p.sub {
	case "sessions":
		run(p.socket, protocolRequest{Action: "sessions"}, false)
	case "stop":
		run(p.socket, protocolRequest{Action: "stop"}, false)
	case "trace":
		fs := flag.NewFlagSet("trace", flag.ExitOnError)
		level := fs.String("trace", first(p.trace, "full"), "Error traceback level: short, smart, or full")
		project := fs.String("project", p.project, "Julia project directory")
		session := fs.String("session", p.session, "Named session label")
		fs.Parse(p.subArgs)
		cmdTrace(p.socket, *project, *session, *level)
	case "interrupt":
		fs := flag.NewFlagSet("interrupt", flag.ExitOnError)
		project := fs.String("project", p.project, "Julia project directory")
		session := fs.String("session", p.session, "Named session label")
		fs.Parse(p.subArgs)
		cmdInterrupt(p.socket, *project, *session)
	case "daemon":
		fs := flag.NewFlagSet("daemon", flag.ExitOnError)
		idleTimeout := fs.Float64("idle-timeout", 60*60, "Idle timeout in seconds")
		fs.Parse(p.subArgs)
		if err := serveDaemon(p.socket, time.Duration(float64(time.Second)**idleTimeout), juliaAdapter{}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
}
