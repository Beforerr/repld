package main

import (
	"cmp"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

var defaultSocket = defaultSocketPath()

func startDaemon(socketPath string) {
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
			return nil, fmt.Errorf("repld daemon is not running (no socket at %s)", socketPath)
		}
		if attempt == 0 {
			startDaemon(socketPath)
		}
		time.Sleep(600 * time.Millisecond)
	}
	return nil, fmt.Errorf("could not connect to repld daemon at %s after startup — try running 'repld daemon' to see errors", socketPath)
}

type response struct {
	Output string `json:"output"`
	Error  string `json:"error"`
}

type protocolRequest struct {
	Action          string   `json:"action"`
	ID              string   `json:"id,omitempty"` // short session id for targeting commands
	Lang            string   `json:"lang,omitempty"`
	Code            string   `json:"code,omitempty"`
	Cwd             string   `json:"cwd,omitempty"`
	Session         string   `json:"session,omitempty"`
	Args            []string `json:"args,omitempty"`
	Exe             string   `json:"exe,omitempty"`
	PrintResult     bool     `json:"print_result,omitempty"`
	Fresh           bool     `json:"fresh,omitempty"`
	TraceLevel      string   `json:"trace_level,omitempty"`
	RequireExisting bool     `json:"require_existing,omitempty"`
	File            string   `json:"file,omitempty"`
	FileArgs        []string `json:"file_args,omitempty"`
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
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
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

func cmdEval(socketPath, lang, code, exe, session string, printResult, fresh bool, traceLevel string, args []string) {
	if code == "-" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		code = string(b)
	}
	req := protocolRequest{
		Action:          "eval",
		Lang:            lang,
		Code:            code,
		Cwd:             mustGetwd(),
		Session:         session,
		Exe:             exe,
		TraceLevel:      traceLevel,
		Args:            args,
		PrintResult:     printResult,
		Fresh:           fresh,
		RequireExisting: lang == "" && exe == "" && session != "",
	}
	run(socketPath, req, true)
}

// cmdEvalFile sends an in-session file eval: path abs-ified but not read here —
// the interpreter reads it at eval time, so edits between calls take effect.
func cmdEvalFile(socketPath, lang, file, exe, session string, fresh bool, traceLevel string, fileArgs, fwd []string) {
	abs, err := filepath.Abs(file)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	run(socketPath, protocolRequest{
		Action:     "eval",
		Lang:       lang,
		Cwd:        mustGetwd(),
		Session:    session,
		Exe:        exe,
		TraceLevel: traceLevel,
		Args:       fwd,
		Fresh:      fresh,
		File:       abs,
		FileArgs:   fileArgs,
	}, true)
}

func cmdInterrupt(socketPath string, tg subTarget, exe string) {
	run(socketPath, protocolRequest{
		Action:  "interrupt",
		ID:      tg.id,
		Lang:    tg.lang,
		Exe:     exe,
		Cwd:     mustGetwd(),
		Session: tg.session,
		Args:    tg.fwd,
	}, false)
}

func cmdClose(socketPath string, tg subTarget, exe string) {
	run(socketPath, protocolRequest{
		Action:  "close",
		ID:      tg.id,
		Lang:    tg.lang,
		Exe:     exe,
		Cwd:     mustGetwd(),
		Session: tg.session,
		Args:    tg.fwd,
	}, false)
}

func cmdTrace(socketPath string, tg subTarget, exe string) {
	run(socketPath, protocolRequest{
		Action:     "trace",
		ID:         tg.id,
		Lang:       tg.lang,
		Exe:        exe,
		Cwd:        mustGetwd(),
		Session:    tg.session,
		TraceLevel: cmp.Or(tg.level, "full"),
		Args:       tg.fwd,
	}, false)
}

func usage(exitCode int) {
	fmt.Fprintf(os.Stderr, `repld: persistent REPL daemon for multiple interpreters

Usage:
  repld <exe> [interp-args] (--<eval> CODE | [--] <file> [script-args] | -)
  repld <command> [<exe>|<id>] [--session L]  # target a session: trace, interrupt, close
  repld <command>                       # daemon-wide: sessions, stop, daemon

<exe> is the interpreter to run (julia, python3, R, wolframscript, .venv/bin/python, /path/...).
The language is inferred from its name, or set with --lang. 
repld's own flags must come before <exe>; after it, every flag forwards verbatim to the
interpreter except native eval/print flags (-e/-E for julia, -c for python/wolframscript, -e for R).

repld flags:
  --lang LANG          Force the language (julia, python, r, wolfram) when the exe is ambiguous
  --session LABEL      Named session, reusable without re-specifying the exe
  --fresh              Clear the targeted session before evaluating
  --trace LEVEL        Error traceback level: short, smart, or full (eval default: smart)

Commands (trace/interrupt/close locate a session by [exe], --session, or the
short id shown by 'sessions'; an id prefix works when unambiguous):
  sessions             List active sessions (all languages)
  trace                Print the last saved error traceback for the session
  interrupt            Interrupt the in-flight eval (SIGKILL after 3s if unresponsive)
  close                Kill the session's interpreter and discard its state
  stop                 Stop the daemon
  daemon               Run the daemon in the foreground (normally auto-started)
    --idle-timeout SECS  Shut down after idle (default: 0 = never; use 'stop')

Global flags:
  --socket PATH        Unix socket path (default: %s)
`, defaultSocket)
	os.Exit(exitCode)
}

var subcommands = map[string]bool{
	"sessions": true, "trace": true, "interrupt": true, "close": true, "stop": true, "daemon": true,
}

type parsed struct {
	socket   string
	exe      string
	lang     string
	session  string
	trace    string
	fresh    bool
	evalMode string
	code     string
	file     string
	fileArgs []string
	fwd      []string
	sub      string
	subArgs  []string
}

func flagName(token string) string {
	name, _, _ := strings.Cut(strings.TrimLeft(token, "-"), "=")
	return name
}

func consumeFlagValue(args []string, i int) (value string, next int, ok bool) {
	_, inline, hasEq := strings.Cut(strings.TrimLeft(args[i], "-"), "=")
	if hasEq {
		return inline, i + 1, true
	}
	if i+1 >= len(args) {
		return "", i + 1, false
	}
	return args[i+1], i + 2, true
}

func parseArgs(args []string) parsed {
	p := parsed{socket: defaultSocket}
	repld := map[string]*string{
		"socket": &p.socket, "lang": &p.lang, "session": &p.session, "trace": &p.trace,
	}
	evalModeFor := func(name string) string {
		evalNames, printNames := evalPrintFlags(resolveLang(p))
		if slices.Contains(evalNames, name) {
			return "eval"
		}
		if slices.Contains(printNames, name) {
			return "print"
		}
		return ""
	}

	for i := 0; i < len(args); {
		t := args[i]
		if p.file != "" {
			p.fileArgs = append(p.fileArgs, t)
			i++
			continue
		}
		// "--" marks the next token as the eval file, mirroring
		// `<exe> [switches] -- [programfile] [args...]`.
		if t == "--" && p.exe != "" && p.evalMode == "" && p.sub == "" {
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "missing file after --")
				usage(2)
			}
			f := args[i+1]
			if fi, err := os.Stat(f); err != nil || !fi.Mode().IsRegular() {
				fmt.Fprintf(os.Stderr, "file not found: %s\n", f)
				os.Exit(1)
			}
			p.file, p.fileArgs = f, args[i+2:]
			break
		}
		if strings.HasPrefix(t, "-") {
			name := flagName(t)
			dst, isRepld := repld[name]
			mode := evalModeFor(name)
			if mode != "" || (isRepld && p.exe == "") {
				val, next, ok := consumeFlagValue(args, i)
				if !ok {
					fmt.Fprintf(os.Stderr, "missing value for %s\n", t)
					usage(2)
				}
				if mode != "" {
					p.evalMode, p.code = mode, val
				} else {
					*dst = val
				}
				i = next
				continue
			}
			if name == "fresh" && p.exe == "" {
				p.fresh = true
				i++
				continue
			}
			p.fwd = append(p.fwd, t)
			i++
			continue
		}
		// File mode (mirrors `<exe> [options] programfile args...`)
		if p.exe != "" && p.evalMode == "" && !subcommands[t] {
			if fi, err := os.Stat(t); (err == nil && fi.Mode().IsRegular()) || (len(p.fwd) == 0 && !strings.HasPrefix(t, "+")) {
				p.file = t
				i++
				continue
			}
		}
		if p.exe == "" && p.evalMode == "" && p.sub == "" && len(p.fwd) == 0 && !subcommands[t] {
			p.exe = t
		} else if p.sub == "" && p.evalMode == "" && len(p.fwd) == 0 && subcommands[t] {
			p.sub, p.subArgs = t, args[i+1:]
			break
		} else {
			p.fwd = append(p.fwd, t)
		}
		i++
	}
	return p
}

func resolveLang(p parsed) string {
	return cmp.Or(p.lang, langForExe(p.exe))
}

func resolveExeStr(exe, lang string) string {
	if exe == "" {
		if lc, ok := langs[lang]; ok {
			exe = lc.adapter.DefaultExe()
		}
	}
	return absExe(exe)
}

type subTarget struct {
	exe, id, lang, session, level string
	fwd                           []string
}

func looksLikeID(s string) bool {
	if len(s) < 2 || len(s) > 2*idLen {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune(idAlphabet, c) {
			return false
		}
	}
	return true
}

func parseTarget(p parsed) subTarget {
	tg := subTarget{lang: p.lang, session: p.session, level: cmp.Or(p.trace, "full")}
	a := p.subArgs
	for i := 0; i < len(a); {
		s := a[i]
		if !strings.HasPrefix(s, "-") {
			if tg.id == "" && tg.exe == "" && looksLikeID(s) {
				tg.id = s
			} else if tg.exe == "" && s != "" {
				tg.exe = s
			} else {
				tg.fwd = append(tg.fwd, s)
			}
			i++
			continue
		}

		switch flagName(s) {
		case "session":
			tg.session, i, _ = consumeFlagValue(a, i)
		case "lang":
			tg.lang, i, _ = consumeFlagValue(a, i)
		case "trace":
			tg.level, i, _ = consumeFlagValue(a, i)
		default:
			tg.fwd = append(tg.fwd, s)
			i++
		}
	}
	tg.lang = cmp.Or(tg.lang, langForExe(tg.exe))
	return tg
}

func absExe(exe string) string {
	if exe == "" || filepath.IsAbs(exe) {
		return exe
	}
	if strings.ContainsRune(exe, os.PathSeparator) || strings.HasPrefix(exe, ".") {
		if a, err := filepath.Abs(exe); err == nil {
			return a
		}
	}
	return exe
}

func main() {
	p := parseArgs(os.Args[1:])

	lang := resolveLang(p)

	if p.sub != "" {
		dispatchSubcommand(p)
		return
	}

	if lang == "" && p.session == "" {
		usage(0)
	}
	exe := resolveExeStr(p.exe, lang)

	switch {
	case p.evalMode != "":
		cmdEval(p.socket, lang, p.code, exe, p.session, p.evalMode == "print", p.fresh, p.trace, p.fwd)
	case p.file != "":
		cmdEvalFile(p.socket, lang, p.file, exe, p.session, p.fresh, p.trace, p.fileArgs, p.fwd)
	case len(p.fwd) > 0:
		cmdEval(p.socket, lang, "", exe, p.session, false, p.fresh, p.trace, p.fwd)
	default:
		fi, err := os.Stdin.Stat()
		if err != nil || fi.Mode()&os.ModeCharDevice != 0 {
			usage(2)
		}
		cmdEval(p.socket, lang, "-", exe, p.session, false, p.fresh, p.trace, p.fwd)
	}
}

func dispatchSubcommand(p parsed) {
	switch p.sub {
	case "sessions":
		run(p.socket, protocolRequest{Action: "sessions"}, false)
	case "stop":
		if !pingDaemon(p.socket) {
			fmt.Println("repld daemon is not running.")
			return
		}
		run(p.socket, protocolRequest{Action: "stop"}, false)
	case "trace":
		tg := parseTarget(p)
		cmdTrace(p.socket, tg, resolveExeStr(tg.exe, tg.lang))
	case "interrupt":
		tg := parseTarget(p)
		cmdInterrupt(p.socket, tg, resolveExeStr(tg.exe, tg.lang))
	case "close":
		tg := parseTarget(p)
		cmdClose(p.socket, tg, resolveExeStr(tg.exe, tg.lang))
	case "daemon":
		fs := flag.NewFlagSet("daemon", flag.ExitOnError)
		idleTimeout := fs.Float64("idle-timeout", 0, "Shut down after this many idle seconds (0 = never)")
		fs.Parse(p.subArgs)
		if err := serveDaemon(p.socket, time.Duration(float64(time.Second)**idleTimeout)); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
}
