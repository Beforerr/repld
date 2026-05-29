package main

import (
	"bufio"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

//go:embed julia_client_runtime.jl
var juliaClientRuntime string

const startupTimeout = 120.0

// JuliaSession manages a single persistent Julia subprocess.
type JuliaSession struct {
	projectVal string // pre-computed --project= arg (also used for display)
	sentinel   string
	juliaArgs  []string // switches forwarded to the julia subprocess

	proc       *exec.Cmd
	stdin      io.WriteCloser
	stdout     *bufio.Reader
	stderr     *bufio.Reader
	control    *bufio.Reader  // fd 3: framed eval status/error from the runtime
	interruptW io.WriteCloser // fd 4: write a byte to interrupt the in-flight eval
	mu         sync.Mutex

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
		projectVal: projectVal,
		sentinel:   sentinel,
		juliaArgs:  juliaArgs,
		logFile:    logFile,
	}
}

func (s *JuliaSession) start(workDir string) error {
	exe, err := exec.LookPath("julia")
	if err != nil {
		return fmt.Errorf("'julia' not found in PATH. Install Julia from https://julialang.org/downloads/")
	}

	// A juliaup channel (+x) must be Julia's very first argument; keep it there.
	var args []string
	rest := s.juliaArgs
	if len(rest) > 0 && strings.HasPrefix(rest[0], "+") {
		args = append(args, rest[0])
		rest = rest[1:]
	}
	args = append(args, "-i")
	args = append(args, rest...)
	args = append(args, fmt.Sprintf("--project=%s", s.projectVal))

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
	// Control channel: user output stays on the real stdout/stderr pipes (so we
	// capture everything, including C-level/subprocess writes), while the
	// runtime reports structured eval status/errors out-of-band on fd 3.
	ctrlR, ctrlW, err := pipe()
	if err != nil {
		closeAll()
		return err
	}
	// Interrupt channel: child reads fd 4 (see interrupt()).
	intR, intW, err := pipe()
	if err != nil {
		closeAll()
		return err
	}
	cmd.Stdout = outW
	cmd.Stderr = errW
	cmd.ExtraFiles = []*os.File{ctrlW, intR} // become fd 3 (write) and fd 4 (read) in the child

	if err := cmd.Start(); err != nil {
		closeAll()
		return err
	}
	outW.Close()
	errW.Close()
	ctrlW.Close() // child holds the only write end now
	intR.Close()  // child holds the only read end now

	s.proc = cmd
	s.stdin = stdin
	s.stdout = bufio.NewReaderSize(outR, 64*1024*1024)
	s.stderr = bufio.NewReaderSize(errR, 64*1024*1024)
	s.control = bufio.NewReader(ctrlR)
	s.interruptW = intW

	// Wait for Julia's interactive prompt to appear
	if _, err := s.executeRaw("", nil, false, startupTimeout); err != nil {
		return fmt.Errorf("Julia startup failed: %w", err)
	}
	runtimeHex := hex.EncodeToString([]byte(juliaClientRuntime))
	if _, err := s.executeRaw(fmt.Sprintf(`include_string(Main, String(hex2bytes("%s")), "julia-client runtime")`, runtimeHex), nil, false, startupTimeout); err != nil {
		return fmt.Errorf("failed to load julia-client runtime: %w", err)
	}
	return nil
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
		s.proc.Wait()
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
	wrapped := fmt.Sprintf(`Main.JuliaClientRuntime.run("%s", %t)`, hexCode, printResult)

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

func (s *JuliaSession) kill() {
	s.dead.Store(true)
	if s.proc != nil && s.proc.Process != nil {
		s.proc.Process.Kill()
		s.proc.Wait()
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
		if s.interruptW != nil {
			_, werr := s.interruptW.Write([]byte{'\n'})
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
			s.proc.Wait()
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
