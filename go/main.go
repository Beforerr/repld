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
	Action      string `json:"action"`
	Code        string `json:"code,omitempty"`
	Cwd         string `json:"cwd,omitempty"`
	Project     string `json:"project,omitempty"`
	Session     string `json:"session,omitempty"`
	JuliaCmd    string `json:"julia_cmd,omitempty"`
	PrintResult bool   `json:"print_result,omitempty"`
	Fresh       bool   `json:"fresh,omitempty"`
	TraceLevel  string `json:"trace_level,omitempty"`
}

// streamFrame is one wire frame in a streaming eval reply. Non-final frames
// carry a chunk; the final frame has Done=true and optional Error.
type streamFrame struct {
	Chunk string `json:"chunk,omitempty"`
	Done  bool   `json:"done,omitempty"`
	Error string `json:"error,omitempty"`
}

// run dials the daemon, sends req, and writes the result to stdout/stderr.
// For eval the daemon replies with NDJSON streaming frames; chunks are
// written to stdout as they arrive. Other actions reply with a single frame.
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
			fmt.Print(f.Chunk)
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

func cmdEval(socketPath, code, project, session, juliaCmd string, printResult, fresh bool, traceLevel string) {
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
		TraceLevel:  traceLevel,
		JuliaCmd:    juliaCmd,
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
  julia-client [flags] [file] [-e CODE]
  julia-client [--socket PATH] <command> [options]

Eval flags:
  -e, --eval CODE      Evaluate Julia code (omit or use - to read stdin)
  -E, --print CODE     Evaluate Julia code and display the result
  --project PROJECT    Julia project directory or selector (passed as --project to Julia)
  --session LABEL      Named session to create or reuse across directories
  --fresh              Clear the targeted session before evaluating
  --julia-cmd CMD      Custom Julia binary, e.g. "julia +1.11"
  --trace LEVEL        Error traceback level: short, smart, or full (eval default: smart)

Session routing (priority order):
  --session LABEL      Shared by label, regardless of directory
  --project PROJECT    Keyed by project path or selector
  (default)            Keyed by current working directory; Julia uses --project=@.

Commands:
  sessions             List active Julia sessions
  trace                Print the last saved Julia error traceback for this session
  interrupt            Send SIGINT (then SIGKILL after 3s) to the targeted session
  stop                 Stop the daemon
  daemon               Run the daemon in the foreground (normally auto-started)
    --idle-timeout SECS  Shut down after idle (default: 3600)

Global flags:
  --socket PATH        Unix socket path (default: %s)
`, defaultSocket)
	os.Exit(2)
}

func main() {
	socketFlag := flag.String("socket", defaultSocket, "Unix socket path")
	evalShort := flag.String("e", "", "Evaluate Julia code")
	evalLong := flag.String("eval", "", "Evaluate Julia code")
	printShort := flag.String("E", "", "Evaluate and display result")
	printLong := flag.String("print", "", "Evaluate and display result")
	projectFlag := flag.String("project", "@.", "Julia project directory")
	sessionFlag := flag.String("session", "", "Named session label")
	freshFlag := flag.Bool("fresh", false, "Clear the targeted session before evaluating")
	juliaCmdFlag := flag.String("julia-cmd", "", "Custom Julia binary")
	traceFlag := flag.String("trace", "", "Error traceback level: short, smart, or full")
	flag.Usage = usage
	flag.Parse()

	// -E / --print: evaluate and display result
	if code := first(*printShort, *printLong); code != "" {
		cmdEval(*socketFlag, code, *projectFlag, *sessionFlag, *juliaCmdFlag, true, *freshFlag, *traceFlag)
		return
	}

	// -e / --eval: evaluate mode
	code := first(*evalShort, *evalLong)
	if code != "" {
		cmdEval(*socketFlag, code, *projectFlag, *sessionFlag, *juliaCmdFlag, false, *freshFlag, *traceFlag)
		return
	}

	args := flag.Args()

	// No subcommand: read stdin only if it's a pipe/redirect, not a terminal
	if len(args) == 0 {
		fi, err := os.Stdin.Stat()
		if err != nil || fi.Mode()&os.ModeCharDevice != 0 {
			usage()
		}
		cmdEval(*socketFlag, "-", *projectFlag, *sessionFlag, *juliaCmdFlag, false, *freshFlag, *traceFlag)
		return
	}

	switch args[0] {
	case "sessions":
		run(*socketFlag, protocolRequest{Action: "sessions"}, false)

	case "trace":
		fs := flag.NewFlagSet("trace", flag.ExitOnError)
		traceLevel := fs.String("trace", first(*traceFlag, "full"), "Error traceback level: short, smart, or full")
		traceProject := fs.String("project", *projectFlag, "Julia project directory")
		traceSession := fs.String("session", *sessionFlag, "Named session label")
		fs.Parse(args[1:])
		cmdTrace(*socketFlag, *traceProject, *traceSession, *traceLevel)

	case "interrupt":
		fs := flag.NewFlagSet("interrupt", flag.ExitOnError)
		intProject := fs.String("project", *projectFlag, "Julia project directory")
		intSession := fs.String("session", *sessionFlag, "Named session label")
		fs.Parse(args[1:])
		cmdInterrupt(*socketFlag, *intProject, *intSession)

	case "stop":
		run(*socketFlag, protocolRequest{Action: "stop"}, false)

	case "daemon":
		fs := flag.NewFlagSet("daemon", flag.ExitOnError)
		idleTimeout := fs.Float64("idle-timeout", 60*60, "Idle timeout in seconds")
		fs.Parse(args[1:])
		if err := serveDaemon(*socketFlag, time.Duration(float64(time.Second)**idleTimeout)); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}

	default:
		if _, err := os.Stat(args[0]); err == nil {
			code := fmt.Sprintf("cd(%q) do; Base.include(Main, %q); end", mustGetwd(), args[0])
			cmdEval(*socketFlag, code, *projectFlag, *sessionFlag, *juliaCmdFlag, false, *freshFlag, *traceFlag)
			return
		}
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", args[0])
		usage()
	}
}
