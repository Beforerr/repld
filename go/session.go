package main

import (
	"bufio"
	"context"
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
	"time"
)

const startupTimeout = 120.0

type Session struct {
	adapter  Adapter
	lang     string
	sentinel string
	fwd      []string

	proc        *exec.Cmd
	stdin       io.WriteCloser
	stdout      *bufio.Reader
	stderr      *bufio.Reader
	control     *bufio.Reader
	controlConn net.Conn
	exited      chan struct{}
	mu          sync.Mutex      // guards startup
	sem         chan struct{}   // capacity-1: serialises evals, ctx-cancellable acquire

	dead      atomic.Bool
	busySince atomic.Int64
	logFile   *os.File
	startup   []startupChunk

	controlAcceptTimeout float64 // seconds to wait for the runtime's control dial-back
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
		adapter:              adapter,
		sentinel:             sentinel,
		fwd:                  fwd,
		logFile:              logFile,
		controlAcceptTimeout: startupTimeout,
		sem:                  make(chan struct{}, 1),
	}
}

func (s *Session) start(exe string, workDir string) error {
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

	// Loopback socket keeps control portable; Windows lacks exec.Cmd.ExtraFiles.
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
	s.stdout = bufio.NewReaderSize(outR, 64*1024)
	s.stderr = bufio.NewReaderSize(errR, 64*1024)

	s.exited = make(chan struct{})
	go func() { cmd.Wait(); close(s.exited) }()

	// The runtime dials back only after load; startup cannot block on Accept.
	controlReady := make(chan struct{})
	go func() {
		defer close(controlReady)
		if conn, br := acceptControl(ln, token, s.controlAcceptTimeout); conn != nil {
			s.controlConn = conn
			s.control = br
		}
	}()

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
		s.kill()
		return fmt.Errorf("interpreter startup failed: %w", err)
	}
	s.startup = startup
	runtimeHex := hex.EncodeToString([]byte(s.adapter.RuntimeSource()))
	if _, err := s.executeRaw(s.adapter.LoadRuntimeStmt(runtimeHex), nil, false, startupTimeout); err != nil {
		s.kill()
		return fmt.Errorf("failed to load runtime: %w", err)
	}
	<-controlReady
	// Without the control channel every eval would fail with "control channel
	// unavailable" yet the session would stay alive and keep being reused (no
	// self-heal). Fail startup instead so the next call recreates it.
	if s.control == nil {
		s.kill()
		return fmt.Errorf("control channel handshake did not complete")
	}
	return nil
}

func stripPythonPrompts(data string) string {
	for strings.HasSuffix(data, ">>> ") || strings.HasSuffix(data, "... ") {
		data = data[:len(data)-4]
	}
	return data
}

type controlWinner struct {
	conn net.Conn
	br   *bufio.Reader
}

// Stray connection (port scanner, other process) can land before runtime dials back;
// accepting only once would consume it and fail the handshake while the real
// dial waits in the backlog.
func acceptControl(ln net.Listener, token string, timeoutSecs float64) (net.Conn, *bufio.Reader) {
	deadline := time.Now().Add(time.Duration(timeoutSecs * float64(time.Second)))
	if tl, ok := ln.(*net.TCPListener); ok {
		tl.SetDeadline(deadline)
	}
	won := make(chan controlWinner, 1)
	authed := func(conn net.Conn) {
		br := bufio.NewReaderSize(conn, 64*1024)
		rd := deadline
		if soon := time.Now().Add(2 * time.Second); soon.Before(rd) {
			rd = soon
		}
		conn.SetReadDeadline(rd)
		line, err := br.ReadString('\n')
		if err != nil || strings.TrimRight(line, "\r\n") != token {
			conn.Close()
			return
		}
		conn.SetReadDeadline(time.Time{})
		select {
		case won <- controlWinner{conn, br}:
			ln.Close()
		default:
			conn.Close()
		}
	}
	for {
		conn, err := ln.Accept()
		if err != nil {
			break // deadline, or listener closed by the winning goroutine
		}
		go authed(conn)
		// A winner closes ln, so next Accept returns and ends loop
		select {
		case w := <-won:
			return w.conn, w.br
		default:
		}
	}
	select {
	case w := <-won:
		return w.conn, w.br
	default:
		return nil, nil
	}
}

func (s *Session) isAlive() bool {
	return !s.dead.Load()
}

func (s *Session) drainStartup() []startupChunk {
	s.mu.Lock()
	defer s.mu.Unlock()
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

func parseControlLine(line string) *evalError {
	line = strings.TrimRight(line, "\r\n")
	rest, ok := strings.CutPrefix(line, "ERR ")
	if !ok {
		return nil
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

func (s *Session) executeRaw(code string, onChunk func(data string, isStderr bool), expectControl bool, timeoutSecs float64) (*evalError, error) {
	if expectControl && s.control == nil {
		return nil, fmt.Errorf("control channel unavailable")
	}

	// The JSON encoder and log writer are shared between stdout/stderr readers.
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

	// Read while the runtime writes; large tracebacks can otherwise deadlock
	// before sentinel drain completes.
	ctrlCh := make(chan *evalError, 1)
	if expectControl {
		go func() {
			line, err := s.control.ReadString('\n')
			if err != nil {
				ctrlCh <- nil
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
		<-doneCh
		return nil, fmt.Errorf("Execution timed out after %vs. Session killed; it will restart on next call.", timeoutSecs)
	}
}

// execute runs one eval to completion. Binding cancellation to ctx (caller's lifetime)
func (s *Session) execute(ctx context.Context, code string, printResult bool, onChunk func(data string, isStderr bool)) error {
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-s.sem }()

	if s.dead.Load() {
		return fmt.Errorf("session has died unexpectedly")
	}

	evalDone := make(chan struct{})
	defer close(evalDone)
	go func() {
		select {
		case <-ctx.Done():
			s.interrupt(3.0)
		case <-evalDone:
		}
	}()

	hexCode := hex.EncodeToString([]byte(code))
	wrapped := s.adapter.WrapEval(hexCode, printResult)

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
	shutdownGrace = 5 * time.Second
	termGrace     = 2 * time.Second
)

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

func (s *Session) interrupt(graceSecs float64) (survived bool, idle bool, err error) {
	if !s.isAlive() {
		return false, false, fmt.Errorf("session is not alive")
	}
	if s.proc == nil || s.proc.Process == nil {
		return false, false, fmt.Errorf("session has no process")
	}
	if s.busySince.Load() == 0 {
		return true, true, nil
	}
	// A single interrupt can be silently lost: scheduling the InterruptException
	// onto the eval task races with the wakeup of whatever it's blocked on, and
	// exception sometimes never lands.
	useControl := s.controlConn != nil && s.adapter.InterruptViaControl()
	signal := func() error {
		if useControl {
			_, werr := s.controlConn.Write([]byte{'\n'})
			return werr
		}
		return interruptProc(s.proc.Process)
	}
	_ = signal()
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
			return false, false, nil
		case <-resend.C:
			if s.busySince.Load() != 0 {
				_ = signal()
			}
		case <-poll.C:
			if s.busySince.Load() == 0 {
				return s.isAlive(), false, nil
			}
		}
	}
}
