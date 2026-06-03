package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const startupTimeout = 120.0

// Session manages a single persistent interpreter subprocess.
type Session struct {
	adapter  Adapter
	lang     string // language name, for the sessions listing
	sentinel string
	fwd      []string // switches forwarded to the interpreter

	proc        *exec.Cmd
	stdin       io.WriteCloser
	stdout      *bufio.Reader
	stderr      *bufio.Reader
	control     *bufio.Reader // loopback control conn: framed eval status/error read from the runtime
	controlConn net.Conn      // same conn; write a byte on it to interrupt the in-flight eval
	exited      chan struct{} // closed once by the reaper when proc exits; the single Wait()
	mu          sync.Mutex

	dead      atomic.Bool
	busySince atomic.Int64 // UnixNano of current call start; 0 when idle
	logFile   *os.File
	startup   []startupChunk
}

type startupChunk struct {
	data     string
	isStderr bool
}

func newSentinel() string {
	b := make([]byte, 16)
	rand.Read(b)
	return fmt.Sprintf("__REPLD_%s__", hex.EncodeToString(b))
}

func newSession(adapter Adapter, sentinel string, fwd []string, logFile *os.File) *Session {
	return &Session{
		adapter:  adapter,
		sentinel: sentinel,
		fwd:      fwd,
		logFile:  logFile,
	}
}

func (s *Session) start(exe string, workDir string) error {
	// exe is an abs-ified path or a bare name looked up in PATH
	if exe == "" {
		exe = s.adapter.DefaultExe()
	}
	if !filepath.IsAbs(exe) {
		resolved, err := exec.LookPath(exe)
		if err != nil {
			return fmt.Errorf("'%s' not found in PATH", exe)
		}
		exe = resolved
	}

	args := s.adapter.LaunchArgs(s.fwd)
	cmd := exec.Command(exe, args...)
	cmd.Dir = workDir

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}

	// Track every pipe end so any failure below can close them in one shot.
	var opened []*os.File
	pipe := func() (r, w *os.File, err error) {
		r, w, err = os.Pipe()
		if err == nil {
			opened = append(opened, r, w)
		}
		return
	}
	closeAll := func() {
		for _, f := range opened {
			f.Close()
		}
	}

	outR, outW, err := pipe()
	if err != nil {
		return err
	}
	errR, errW, err := pipe()
	if err != nil {
		closeAll()
		return err
	}
	cmd.Stdout = outW
	cmd.Stderr = errW

	// Control channel: a loopback socket the runtime dials back on, carrying
	// framed eval status (child→parent) and interrupt bytes (parent→child) out
	// of band from user stdout/stderr. A socket rather than inherited fds
	// keeps the protocol portable to Windows.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		closeAll()
		return err
	}
	defer ln.Close()
	token := newSentinel()
	cmd.Env = append(os.Environ(),
		"REPLD_CONTROL_ADDR="+ln.Addr().String(),
		"REPLD_CONTROL_TOKEN="+token,
	)

	if err := cmd.Start(); err != nil {
		closeAll()
		return err
	}
	outW.Close()
	errW.Close()

	s.proc = cmd
	s.stdin = stdin
	s.stdout = bufio.NewReaderSize(outR, 64*1024*1024)
	s.stderr = bufio.NewReaderSize(errR, 64*1024*1024)

	// Reap the process exactly once; everything that needs to wait for exit
	// reads s.exited rather than calling Wait() again (which would error/race).
	s.exited = make(chan struct{})
	go func() { cmd.Wait(); close(s.exited) }()

	// The child only dials back once it loads the runtime (below), so accept
	// concurrently; a failed connection degrades to no control channel.
	controlReady := make(chan struct{})
	go func() {
		defer close(controlReady)
		if conn, br := acceptControl(ln, token, startupTimeout); conn != nil {
			s.controlConn = conn
			s.control = br
		}
	}()

	// Capture all startup output (a forwarded program file runs here, before the
	// runtime loads) so it can be streamed to the first client.
	var startup []startupChunk
	capture := func(data string, isStderr bool) {
		if s.lang == "python" && isStderr {
			data = stripPythonPrompts(data)
			if data == "" {
				return
			}
		}
		startup = append(startup, startupChunk{data: data, isStderr: isStderr})
	}
	if _, err := s.executeRaw("", capture, false, startupTimeout); err != nil {
		return fmt.Errorf("interpreter startup failed: %w", err)
	}
	s.startup = startup
	runtimeHex := hex.EncodeToString([]byte(s.adapter.RuntimeSource()))
	if _, err := s.executeRaw(s.adapter.LoadRuntimeStmt(runtimeHex), nil, false, startupTimeout); err != nil {
		return fmt.Errorf("failed to load runtime: %w", err)
	}
	<-controlReady // control is established (or degraded) before the first eval
	return nil
}

func stripPythonPrompts(data string) string {
	for strings.HasSuffix(data, ">>> ") || strings.HasSuffix(data, "... ") {
		data = data[:len(data)-4]
	}
	return data
}

// acceptControl waits for the child to dial back on ln and present the shared
// token as its first line, returning the verified connection and a buffered
// reader over it. Returns nil if the child never connects in time or the token
// is wrong, in which case control degrades to "no structured error".
func acceptControl(ln net.Listener, token string, timeoutSecs float64) (net.Conn, *bufio.Reader) {
	deadline := time.Now().Add(time.Duration(timeoutSecs * float64(time.Second)))
	if tl, ok := ln.(*net.TCPListener); ok {
		tl.SetDeadline(deadline)
	}
	conn, err := ln.Accept()
	if err != nil {
		return nil, nil
	}
	br := bufio.NewReaderSize(conn, 64*1024*1024)
	conn.SetReadDeadline(deadline)
	line, err := br.ReadString('\n')
	if err != nil || strings.TrimRight(line, "\r\n") != token {
		conn.Close()
		return nil, nil
	}
	conn.SetReadDeadline(time.Time{}) // frames arrive whenever an eval runs
	return conn, br
}

func (s *Session) isAlive() bool {
	return !s.dead.Load()
}

func (s *Session) drainStartup() []startupChunk {
	chunks := s.startup
	s.startup = nil
	return chunks
}

type evalError struct {
	short string
	smart string
	full  string
}

func (e *evalError) Error() string {
	return e.short
}

func decodeHexString(s string) (string, error) {
	b, err := hex.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// parseControlLine decodes one control frame: "OK", or "ERR <hex short> <hex
// smart> <hex full>". Malformed/empty degrades to no structured error.
func parseControlLine(line string) *evalError {
	line = strings.TrimRight(line, "\r\n")
	rest, ok := strings.CutPrefix(line, "ERR ")
	if !ok {
		return nil // "OK", "", or unrecognized
	}
	parts := strings.Split(rest, " ")
	if len(parts) != 3 {
		return nil
	}
	short, e1 := decodeHexString(parts[0])
	smart, e2 := decodeHexString(parts[1])
	full, e3 := decodeHexString(parts[2])
	if e1 != nil || e2 != nil || e3 != nil {
		return nil
	}
	return &evalError{short: short, smart: smart, full: full}
}

// scanToSentinel relays r line by line to emit until a line ending in the
// sentinel (the drain barrier), then returns. On EOF/error before the sentinel
// it returns that error. It keeps a bounded tail of recent output for the
// caller's "died during execution" diagnostic.
func (s *Session) scanToSentinel(r *bufio.Reader, isStderr bool, emit func(string, bool)) (tail string, err error) {
	const maxTail = 64 * 1024
	var buf []byte
	keep := func(s string) {
		buf = append(buf, s...)
		if len(buf) > maxTail {
			buf = append(buf[:0], buf[len(buf)-maxTail:]...)
		}
	}
	for {
		line, rerr := r.ReadString('\n')
		raw := strings.TrimRight(line, "\r\n")
		if strings.HasSuffix(raw, s.sentinel) {
			if prefix := strings.TrimSuffix(raw, s.sentinel); prefix != "" {
				keep(prefix)
				if emit != nil {
					emit(prefix, isStderr)
				}
			}
			return string(buf), nil
		}
		if rerr != nil {
			return string(buf), rerr
		}
		keep(line)
		if emit != nil {
			emit(line, isStderr)
		}
	}
}

// executeRaw writes code followed by a sentinel command, relays stdout/stderr
// via onChunk until both sentinels arrive (the drain barrier), and — when
// expectControl is set — reads one framed status/error from fd 3. User output
// is streamed, never retained; only a bounded tail is kept for the
// "died during execution" diagnostic. Returns the eval's error (nil on
// success or when no control frame is expected) and an infra error.
func (s *Session) executeRaw(code string, onChunk func(data string, isStderr bool), expectControl bool, timeoutSecs float64) (*evalError, error) {
	if expectControl && s.control == nil {
		return nil, fmt.Errorf("control channel unavailable")
	}

	// stdout and stderr are read by separate goroutines below, but the
	// consumer (daemon JSON encoder, session log) is not safe for concurrent
	// writes. Serialize delivery so each chunk is emitted atomically; this also
	// gives clients a deterministic frame order when merging the two streams
	// (e.g. shell `2>&1`).
	var emitMu sync.Mutex
	emit := onChunk
	if onChunk != nil {
		emit = func(data string, isStderr bool) {
			emitMu.Lock()
			onChunk(data, isStderr)
			emitMu.Unlock()
		}
	}

	if _, err := io.WriteString(s.stdin, code+"\n"+s.adapter.SentinelStmt(s.sentinel)+"\n"); err != nil {
		return nil, err
	}

	outCh := make(chan error, 1)
	go func() {
		tail, err := s.scanToSentinel(s.stdout, false, emit)
		if err != nil {
			s.dead.Store(true)
			outCh <- fmt.Errorf("interpreter process died during execution.\nOutput before death:\n%s", tail)
			return
		}
		outCh <- nil
	}()

	type scanResult struct {
		tail string
		err  error
	}

	errCh := make(chan scanResult, 1)
	go func() {
		tail, err := s.scanToSentinel(s.stderr, true, emit)
		errCh <- scanResult{tail: tail, err: err}
	}()

	// Drain the control channel concurrently. The runtime flushes the frame
	// before printing the sentinel, but a large frame can exceed the pipe
	// buffer, so we must read it as it's written rather than after the sentinel
	// (which would deadlock). Buffered so the goroutine never leaks if the
	// process dies and we return early.
	ctrlCh := make(chan *evalError, 1)
	if expectControl {
		go func() {
			line, err := s.control.ReadString('\n')
			if err != nil {
				ctrlCh <- nil // control unavailable; degrade to no structured error
				return
			}
			ctrlCh <- parseControlLine(line)
		}()
	}

	wait := func() (*evalError, error) {
		outErr := <-outCh
		errResult := <-errCh
		if outErr != nil {
			if strings.TrimSpace(errResult.tail) != "" {
				return nil, fmt.Errorf("%w\nStderr before death:\n%s", outErr, errResult.tail)
			}
			return nil, outErr
		}
		if expectControl {
			return <-ctrlCh, nil
		}
		return nil, nil
	}

	if timeoutSecs <= 0 {
		return wait()
	}

	timer := time.NewTimer(time.Duration(float64(time.Second) * timeoutSecs))
	defer timer.Stop()

	type rawResult struct {
		evalErr *evalError
		err     error
	}
	doneCh := make(chan rawResult, 1)
	go func() {
		ee, e := wait()
		doneCh <- rawResult{ee, e}
	}()

	select {
	case r := <-doneCh:
		return r.evalErr, r.err
	case <-timer.C:
		s.proc.Process.Kill()
		<-s.exited
		s.dead.Store(true)
		<-doneCh // both goroutines unblock on EOF after kill
		return nil, fmt.Errorf("Execution timed out after %vs. Session killed; it will restart on next call.", timeoutSecs)
	}
}

func (s *Session) execute(code string, printResult bool, onChunk func(data string, isStderr bool)) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.dead.Load() {
		return fmt.Errorf("session has died unexpectedly")
	}

	hexCode := hex.EncodeToString([]byte(code))
	wrapped := s.adapter.WrapEval(hexCode, printResult)

	// Tee both streams to the log as they arrive. executeRaw serializes the
	// callback, so stdout and stderr land interleaved in causal order — the
	// same view a terminal shows under `2>&1`. (The NDJSON protocol keeps them
	// tagged for machine consumers; the log is the human-readable record.)
	sink := onChunk
	if s.logFile != nil {
		fmt.Fprintf(s.logFile, "[%s] %s> %s\n", time.Now().Format("15:04:05"), s.lang, code)
		sink = func(data string, isStderr bool) {
			io.WriteString(s.logFile, data)
			if onChunk != nil {
				onChunk(data, isStderr)
			}
		}
	}

	s.busySince.Store(time.Now().UnixNano())
	defer s.busySince.Store(0)

	evalErr, err := s.executeRaw(wrapped, sink, true, 0)
	if err != nil {
		return err
	}
	if evalErr != nil {
		if s.logFile != nil {
			fmt.Fprintf(s.logFile, "\n%s\n\n", evalErr.full)
		}
		return evalErr
	}
	if s.logFile != nil {
		io.WriteString(s.logFile, "\n\n")
	}
	return nil
}

const (
	shutdownGrace = 5 * time.Second // clean REPL exit after stdin EOF
	termGrace     = 2 * time.Second // SIGTERM grace before SIGKILL
)

// kill shuts the session down, escalating only as far as needed: close stdin so
// the REPL hits EOF and exits cleanly (running atexit hooks and finalizers),
// then SIGTERM, then SIGKILL. controlConn/logFile are closed last, after the
// process is gone, so a clean exit isn't perturbed mid-shutdown.
func (s *Session) kill() {
	s.dead.Store(true)
	defer s.closeResources()

	if s.proc == nil || s.proc.Process == nil {
		return
	}
	if s.stdin != nil {
		s.stdin.Close()
	}
	if s.waitExit(shutdownGrace) {
		return
	}
	terminateProc(s.proc.Process)
	if s.waitExit(termGrace) {
		return
	}
	s.proc.Process.Kill()
	<-s.exited
}

func (s *Session) waitExit(d time.Duration) bool {
	select {
	case <-s.exited:
		return true
	case <-time.After(d):
		return false
	}
}

func (s *Session) closeResources() {
	if s.controlConn != nil {
		s.controlConn.Close()
	}
	if s.logFile != nil {
		s.logFile.Close()
	}
}

// interrupt asks the runtime to interrupt current eval, then escalates to kill.
func (s *Session) interrupt(graceSecs float64) (survived bool, err error) {
	if !s.isAlive() {
		return false, fmt.Errorf("session is not alive")
	}
	if s.proc == nil || s.proc.Process == nil {
		return false, fmt.Errorf("session has no process")
	}
	if s.busySince.Load() == 0 {
		// Nothing to interrupt; treat as no-op success.
		return true, nil
	}
	// A single interrupt can be silently lost: scheduling the InterruptException
	// onto the eval task races with the wakeup of whatever it's blocked on (e.g.
	// the timer behind `sleep`), and the exception sometimes never lands. So we
	// resend on a slow cadence until the call returns. The runtime clears its
	// target task once the eval ends, so any late byte is a no-op; the cadence is
	// well below the listener's drain rate, so no backlog leaks into a next eval.
	signal := func() error {
		if s.controlConn != nil {
			_, werr := s.controlConn.Write([]byte{'\n'})
			return werr
		}
		return s.proc.Process.Signal(syscall.SIGINT)
	}
	if err := signal(); err != nil {
		return false, err
	}
	deadline := time.After(time.Duration(float64(time.Second) * graceSecs))
	poll := time.NewTicker(50 * time.Millisecond)
	defer poll.Stop()
	resend := time.NewTicker(250 * time.Millisecond)
	defer resend.Stop()
	for {
		select {
		case <-deadline:
			s.proc.Process.Kill()
			<-s.exited
			s.dead.Store(true)
			return false, nil
		case <-resend.C:
			if s.busySince.Load() != 0 {
				_ = signal() // a prior interrupt may have been lost to the race
			}
		case <-poll.C:
			if s.busySince.Load() == 0 {
				// Call returned; survived iff process didn't crash on the interrupt.
				return s.isAlive(), nil
			}
		}
	}
}
