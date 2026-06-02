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

// JuliaSession manages a single persistent interpreter subprocess.
type JuliaSession struct {
	adapter    Adapter
	projectVal string // pre-computed --project= arg (also used for display)
	sentinel   string
	juliaArgs  []string // switches forwarded to the subprocess

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
}

func newSentinel() string {
	b := make([]byte, 16)
	rand.Read(b)
	return fmt.Sprintf("__JULIA_CLIENT_%s__", hex.EncodeToString(b))
}

func newJuliaSession(projectVal, sentinel string, juliaArgs []string, logFile *os.File) *JuliaSession {
	return &JuliaSession{
		adapter:    juliaAdapter{},
		projectVal: projectVal,
		sentinel:   sentinel,
		juliaArgs:  juliaArgs,
		logFile:    logFile,
	}
}

func (s *JuliaSession) start(juliaExe string, workDir string) error {
	var exe string
	if juliaExe != "" {
		if filepath.IsAbs(juliaExe) {
			exe = juliaExe
		} else {
			// Relative path: resolved relative to the project directory
			exe = filepath.Join(s.projectVal, juliaExe)
		}
	} else {
		var err error
		exe, err = exec.LookPath(s.adapter.DefaultExe())
		if err != nil {
			return fmt.Errorf("'%s' not found in PATH", s.adapter.DefaultExe())
		}
	}

	args := s.adapter.LaunchArgs(s.projectVal, s.juliaArgs)
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
	// because exec.Cmd.ExtraFiles is unsupported on Windows; the token rejects
	// any other local process that races to the ephemeral port first.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		closeAll()
		return err
	}
	defer ln.Close()
	token := newSentinel()
	cmd.Env = append(os.Environ(),
		"JULIA_CLIENT_CONTROL_ADDR="+ln.Addr().String(),
		"JULIA_CLIENT_CONTROL_TOKEN="+token,
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

	// Wait for Julia's interactive prompt to appear
	if _, err := s.executeRaw("", nil, false, startupTimeout); err != nil {
		return fmt.Errorf("Julia startup failed: %w", err)
	}
	runtimeHex := hex.EncodeToString([]byte(s.adapter.RuntimeSource()))
	if _, err := s.executeRaw(s.adapter.LoadRuntimeStmt(runtimeHex), nil, false, startupTimeout); err != nil {
		return fmt.Errorf("failed to load runtime: %w", err)
	}
	<-controlReady // control is established (or degraded) before the first eval
	return nil
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

func (s *JuliaSession) isAlive() bool {
	return !s.dead.Load()
}

type juliaEvalError struct {
	short string
	smart string
	full  string
}

func (e *juliaEvalError) Error() string {
	return e.short
}

func decodeHexString(s string) (string, error) {
	b, err := hex.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// parseControlLine decodes one control frame from fd 3. The runtime writes
// exactly one per eval: "OK" on success, or "ERR <hex short> <hex smart>
// <hex full>" on a Julia error. A malformed or empty line degrades to "no
// structured error" rather than failing the eval.
func parseControlLine(line string) *juliaEvalError {
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
	return &juliaEvalError{short: short, smart: smart, full: full}
}

// scanToSentinel relays r line by line to emit until a line ending in the
// sentinel (the drain barrier), then returns. On EOF/error before the sentinel
// it returns that error. It keeps a bounded tail of recent output for the
// caller's "died during execution" diagnostic.
func (s *JuliaSession) scanToSentinel(r *bufio.Reader, isStderr bool, emit func(string, bool)) (tail string, err error) {
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
// "died during execution" diagnostic. Returns the eval's Julia error (nil on
// success or when no control frame is expected) and an infra error.
func (s *JuliaSession) executeRaw(code string, onChunk func(data string, isStderr bool), expectControl bool, timeoutSecs float64) (*juliaEvalError, error) {
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

	sentinelCmd := fmt.Sprintf(
		"flush(stderr); println(stderr, \"%s\"); flush(stderr); println(stdout, \"%s\"); flush(stdout)\n",
		s.sentinel, s.sentinel,
	)
	if _, err := io.WriteString(s.stdin, code+"\n"+sentinelCmd); err != nil {
		return nil, err
	}

	outCh := make(chan error, 1)
	go func() {
		tail, err := s.scanToSentinel(s.stdout, false, emit)
		if err != nil {
			s.dead.Store(true)
			outCh <- fmt.Errorf("Julia process died during execution.\nOutput before death:\n%s", tail)
			return
		}
		outCh <- nil
	}()

	errCh := make(chan error, 1)
	go func() {
		_, err := s.scanToSentinel(s.stderr, true, emit)
		errCh <- err // nil at sentinel; EOF surfaced so caller can act
	}()

	// Drain the control channel concurrently. The runtime flushes the frame
	// before printing the sentinel, but a large frame can exceed the pipe
	// buffer, so we must read it as it's written rather than after the sentinel
	// (which would deadlock). Buffered so the goroutine never leaks if the
	// process dies and we return early.
	ctrlCh := make(chan *juliaEvalError, 1)
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

	wait := func() (*juliaEvalError, error) {
		outErr := <-outCh
		<-errCh // stderr reader sees sentinel after stdout's; ignore its terminal err here
		if outErr != nil {
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
		evalErr *juliaEvalError
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

func (s *JuliaSession) execute(code string, printResult bool, onChunk func(data string, isStderr bool)) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.dead.Load() {
		return fmt.Errorf("Julia session has died unexpectedly")
	}

	hexCode := hex.EncodeToString([]byte(code))
	wrapped := s.adapter.WrapEval(hexCode, printResult)

	// Tee both streams to the log as they arrive. executeRaw serializes the
	// callback, so stdout and stderr land interleaved in causal order — the
	// same view a terminal shows under `2>&1`. (The NDJSON protocol keeps them
	// tagged for machine consumers; the log is the human-readable record.)
	sink := onChunk
	if s.logFile != nil {
		fmt.Fprintf(s.logFile, "[%s] julia> %s\n", time.Now().Format("15:04:05"), code)
		sink = func(data string, isStderr bool) {
			io.WriteString(s.logFile, data)
			if onChunk != nil {
				onChunk(data, isStderr)
			}
		}
	}

	s.busySince.Store(time.Now().UnixNano())
	defer s.busySince.Store(0)

	juliaErr, err := s.executeRaw(wrapped, sink, true, 0)
	if err != nil {
		return err
	}
	if juliaErr != nil {
		if s.logFile != nil {
			fmt.Fprintf(s.logFile, "\n%s\n\n", juliaErr.full)
		}
		return juliaErr
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
func (s *JuliaSession) kill() {
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

func (s *JuliaSession) waitExit(d time.Duration) bool {
	select {
	case <-s.exited:
		return true
	case <-time.After(d):
		return false
	}
}

func (s *JuliaSession) closeResources() {
	if s.controlConn != nil {
		s.controlConn.Close()
	}
	if s.logFile != nil {
		s.logFile.Close()
	}
}

// interrupt writes to the fd-4 channel; the runtime turns it into a catchable
// InterruptException on the eval task (see julia_client_runtime.jl). Escalates
// to SIGKILL if the call doesn't return within graceSecs; returns survived=true
// only if the call ended and the process is still alive. Falls back to SIGINT
// when the channel is unavailable (e.g. Windows).
func (s *JuliaSession) interrupt(graceSecs float64) (survived bool, err error) {
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
