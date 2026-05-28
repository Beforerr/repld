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
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sync/singleflight"
)

//go:embed julia_client_runtime.jl
var juliaClientRuntime string

const startupTimeout = 120.0

// JuliaSession manages a single persistent Julia subprocess.
type JuliaSession struct {
	projectVal string // pre-computed --project= arg (also used for display)
	sentinel   string
	juliaCmd   string

	proc   *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr *bufio.Reader
	mu     sync.Mutex

	dead      atomic.Bool
	busySince atomic.Int64 // UnixNano of current call start; 0 when idle
	logFile   *os.File
}

func newSentinel() string {
	b := make([]byte, 16)
	rand.Read(b)
	return fmt.Sprintf("__JULIA_CLIENT_%s__", hex.EncodeToString(b))
}

func newJuliaSession(projectVal, sentinel, juliaCmd string, logFile *os.File) *JuliaSession {
	return &JuliaSession{
		projectVal: projectVal,
		sentinel:   sentinel,
		juliaCmd:   juliaCmd,
		logFile:    logFile,
	}
}

func (s *JuliaSession) start(workDir string) error {
	exe := "julia"
	var channelArgs, extraFlags []string

	if s.juliaCmd != "" {
		parts := strings.Fields(s.juliaCmd)
		exe = parts[0]
		rest := parts[1:]
		if len(rest) > 0 && strings.HasPrefix(rest[0], "+") {
			channelArgs = rest[:1]
			extraFlags = rest[1:]
		} else {
			extraFlags = rest
		}
	}

	if !filepath.IsAbs(exe) {
		resolved, err := exec.LookPath(exe)
		if err != nil {
			return fmt.Errorf("'%s' not found in PATH. Install Julia from https://julialang.org/downloads/", exe)
		}
		exe = resolved
	}

	args := append(channelArgs, "-i", "--threads=auto")
	args = append(args, extraFlags...)
	args = append(args, fmt.Sprintf("--project=%s", s.projectVal))

	cmd := exec.Command(exe, args...)
	cmd.Dir = workDir

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}

	outR, outW, err := os.Pipe()
	if err != nil {
		return err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outW.Close()
		outR.Close()
		return err
	}
	cmd.Stdout = outW
	cmd.Stderr = errW

	if err := cmd.Start(); err != nil {
		outW.Close()
		outR.Close()
		errW.Close()
		errR.Close()
		return err
	}
	outW.Close()
	errW.Close()

	s.proc = cmd
	s.stdin = stdin
	s.stdout = bufio.NewReaderSize(outR, 64*1024*1024)
	s.stderr = bufio.NewReaderSize(errR, 64*1024*1024)

	// Wait for Julia's interactive prompt to appear
	if _, err := s.executeRaw("", nil, startupTimeout); err != nil {
		return fmt.Errorf("Julia startup failed: %w", err)
	}
	runtimeHex := hex.EncodeToString([]byte(juliaClientRuntime))
	if _, err := s.executeRaw(fmt.Sprintf(`include_string(Main, String(hex2bytes("%s")), "julia-client runtime")`, runtimeHex), nil, startupTimeout); err != nil {
		return fmt.Errorf("failed to load julia-client runtime: %w", err)
	}
	return nil
}

func (s *JuliaSession) isAlive() bool {
	return !s.dead.Load()
}

type readResult struct {
	output string
	err    error
}

type juliaEvalError struct {
	short string
	smart string
	full  string
}

func (e *juliaEvalError) Error() string {
	return e.short
}

func (s *JuliaSession) errorStartMarker() string {
	return s.sentinel + "_ERROR_START"
}

func (s *JuliaSession) errorEndMarker() string {
	return s.sentinel + "_ERROR_END"
}

func decodeHexString(s string) (string, error) {
	b, err := hex.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (s *JuliaSession) parseJuliaError(output string) (string, *juliaEvalError) {
	start := s.errorStartMarker()
	idx := strings.Index(output, start+"\n")
	if idx < 0 {
		return output, nil
	}

	prefix := output[:idx]
	rest := output[idx+len(start)+1:]
	parts := strings.SplitN(rest, "\n", 4)
	if len(parts) < 4 {
		return output, nil
	}
	decoded := make([]string, 3)
	for i := range decoded {
		var err error
		decoded[i], err = decodeHexString(parts[i])
		if err != nil {
			return output, nil
		}
	}
	if !strings.HasPrefix(parts[3], s.errorEndMarker()) {
		return output, nil
	}
	return prefix, &juliaEvalError{
		short: decoded[0],
		smart: decoded[1],
		full:  decoded[2],
	}
}

// Returns stdout buffer (for parseJuliaError); stderr streams via onChunk only.
// HasSuffix sentinel detection tolerates unterminated user lines.
func (s *JuliaSession) executeRaw(code string, onChunk func(data string, isStderr bool), timeoutSecs float64) (string, error) {
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
		return "", err
	}

	startMarker := s.errorStartMarker()
	endMarker := s.errorEndMarker()

	outCh := make(chan readResult, 1)
	go func() {
		var buf strings.Builder
		inErrorBlock := false
		for {
			line, err := s.stdout.ReadString('\n')
			raw := strings.TrimRight(line, "\r\n")

			if inErrorBlock {
				buf.WriteString(line)
				if strings.HasPrefix(raw, endMarker) {
					inErrorBlock = false
				}
				if err != nil {
					s.dead.Store(true)
					outCh <- readResult{buf.String(), fmt.Errorf("Julia process died during execution.\nOutput before death:\n%s", buf.String())}
					return
				}
				continue
			}

			if strings.HasSuffix(raw, s.sentinel) {
				if prefix := strings.TrimSuffix(raw, s.sentinel); prefix != "" {
					buf.WriteString(prefix)
					if emit != nil {
						emit(prefix, false)
					}
				}
				outCh <- readResult{buf.String(), nil}
				return
			}
			if strings.HasSuffix(raw, startMarker) {
				if prefix := strings.TrimSuffix(raw, startMarker); prefix != "" {
					buf.WriteString(prefix)
					if emit != nil {
						emit(prefix, false)
					}
				}
				buf.WriteString(startMarker + "\n")
				inErrorBlock = true
				continue
			}

			if err != nil {
				s.dead.Store(true)
				outCh <- readResult{buf.String(), fmt.Errorf("Julia process died during execution.\nOutput before death:\n%s", buf.String())}
				return
			}

			buf.WriteString(line)
			if emit != nil {
				emit(line, false)
			}
		}
	}()

	errCh := make(chan error, 1)
	go func() {
		for {
			line, err := s.stderr.ReadString('\n')
			raw := strings.TrimRight(line, "\r\n")
			if strings.HasSuffix(raw, s.sentinel) {
				if prefix := strings.TrimSuffix(raw, s.sentinel); prefix != "" && emit != nil {
					emit(prefix, true)
				}
				errCh <- nil
				return
			}
			if err != nil {
				// Process died (EOF) — surface this so caller can act.
				errCh <- err
				return
			}
			if emit != nil {
				emit(line, true)
			}
		}
	}()

	wait := func() (string, error) {
		out := <-outCh
		<-errCh // stderr reader sees sentinel after stdout's; ignore its terminal err here
		return out.output, out.err
	}

	if timeoutSecs <= 0 {
		return wait()
	}

	timer := time.NewTimer(time.Duration(float64(time.Second) * timeoutSecs))
	defer timer.Stop()

	doneCh := make(chan readResult, 1)
	go func() {
		o, e := wait()
		doneCh <- readResult{o, e}
	}()

	select {
	case r := <-doneCh:
		return r.output, r.err
	case <-timer.C:
		s.proc.Process.Kill()
		s.proc.Wait()
		s.dead.Store(true)
		r := <-doneCh // both goroutines unblock on EOF after kill
		msg := fmt.Sprintf("Execution timed out after %vs. Session killed; it will restart on next call.", timeoutSecs)
		if r.output != "" {
			msg += "\n\nOutput before timeout:\n" + r.output
		}
		return "", fmt.Errorf("%s", msg)
	}
}

func (s *JuliaSession) execute(code string, printResult bool, onChunk func(data string, isStderr bool)) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.dead.Load() {
		return "", fmt.Errorf("Julia session has died unexpectedly")
	}

	hexCode := hex.EncodeToString([]byte(code))
	wrapped := fmt.Sprintf(`Main.JuliaClientRuntime.run("%s", %t, "%s", "%s")`,
		hexCode, printResult, s.errorStartMarker(), s.errorEndMarker(),
	)

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

	output, err := s.executeRaw(wrapped, sink, 0)
	if err != nil {
		return "", err
	}
	output, juliaErr := s.parseJuliaError(output)
	if juliaErr != nil {
		if s.logFile != nil {
			fmt.Fprintf(s.logFile, "\n%s\n\n", juliaErr.full)
		}
		return output, juliaErr
	}
	if s.logFile != nil {
		io.WriteString(s.logFile, "\n\n")
	}
	return output, nil
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

// interrupt sends SIGINT to the Julia subprocess. If the in-flight call
// (if any) doesn't return within graceSecs, escalates to SIGKILL.
// Returns survived=true only if the call ended and the process is still
// alive. Julia's SIGINT handling is best-effort: it sometimes crashes the
// process even when the interrupt is "successful" from our side.
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
	if err := s.proc.Process.Signal(syscall.SIGINT); err != nil {
		return false, err
	}
	deadline := time.After(time.Duration(float64(time.Second) * graceSecs))
	poll := time.NewTicker(50 * time.Millisecond)
	defer poll.Stop()
	for {
		select {
		case <-deadline:
			s.proc.Process.Kill()
			s.proc.Wait()
			s.dead.Store(true)
			return false, nil
		case <-poll.C:
			if s.busySince.Load() == 0 {
				// Call returned; survived iff process didn't crash on the interrupt.
				return s.isAlive(), nil
			}
		}
	}
}

// SessionManager tracks multiple named Julia sessions.
type SessionManager struct {
	mu         sync.Mutex
	sessions   map[string]*JuliaSession
	lastErrors map[string]*juliaEvalError
	sf         singleflight.Group
	logDir     string
}

func newSessionManager() *SessionManager {
	logDir, _ := os.MkdirTemp("", "julia-client-logs-")
	return &SessionManager{
		sessions:   make(map[string]*JuliaSession),
		lastErrors: make(map[string]*juliaEvalError),
		logDir:     logDir,
	}
}

// key returns the session map key.
// Priority: explicit session label > explicit project path > cwd.
func (m *SessionManager) key(session, project, cwd string) string {
	if session != "" {
		return "~" + session
	}
	if project != "" && project != "@." {
		if strings.HasPrefix(project, "@") {
			return project
		}
		abs, _ := filepath.Abs(project)
		return abs
	}
	return cwd
}

func (m *SessionManager) openLogFile(key string) *os.File {
	safe := strings.NewReplacer("/", "_", "\\", "_").Replace(strings.Trim(key, "/~"))
	if safe == "" {
		safe = "default"
	}
	f, _ := os.OpenFile(filepath.Join(m.logDir, safe+".log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	return f
}

func (m *SessionManager) getOrCreate(cwd, project, session, juliaCmd string) (*JuliaSession, error) {
	key := m.key(session, project, cwd)

	// Fast path: return existing live session without singleflight overhead.
	m.mu.Lock()
	sess := m.sessions[key]
	m.mu.Unlock()
	if sess != nil && sess.isAlive() && sess.juliaCmd == juliaCmd {
		return sess, nil
	}

	// Slow path: deduplicate concurrent creation for the same key.
	v, err, _ := m.sf.Do(key, func() (any, error) {
		m.mu.Lock()
		sess := m.sessions[key]
		m.mu.Unlock()
		if sess != nil && sess.isAlive() && sess.juliaCmd == juliaCmd {
			return sess, nil
		}
		if sess != nil {
			sess.kill()
			m.mu.Lock()
			delete(m.sessions, key)
			m.mu.Unlock()
		}

		projectVal := project
		if projectVal == "" {
			projectVal = "@."
		}
		sess = newJuliaSession(projectVal, newSentinel(), juliaCmd, m.openLogFile(key))
		if err := sess.start(cwd); err != nil {
			return nil, err
		}

		m.mu.Lock()
		m.sessions[key] = sess
		m.mu.Unlock()
		return sess, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*JuliaSession), nil
}

func (m *SessionManager) remove(session, project, cwd string) {
	key := m.key(session, project, cwd)
	m.mu.Lock()
	delete(m.sessions, key)
	m.mu.Unlock()
}

func (m *SessionManager) restart(session, project, cwd string) {
	key := m.key(session, project, cwd)
	m.mu.Lock()
	sess := m.sessions[key]
	delete(m.sessions, key)
	delete(m.lastErrors, key)
	m.mu.Unlock()
	if sess != nil {
		sess.kill()
	}
}

func (m *SessionManager) recordError(session, project, cwd string, err *juliaEvalError) {
	key := m.key(session, project, cwd)
	m.mu.Lock()
	m.lastErrors[key] = err
	m.mu.Unlock()
}

func (m *SessionManager) lastError(session, project, cwd string) *juliaEvalError {
	key := m.key(session, project, cwd)
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastErrors[key]
}

type sessionInfo struct {
	key      string
	project  string
	alive    bool
	juliaCmd string
	logFile  string
	busyFor  time.Duration // 0 when idle
}

func (m *SessionManager) list() []sessionInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	result := make([]sessionInfo, 0, len(m.sessions))
	for key, sess := range m.sessions {
		info := sessionInfo{
			key:      key,
			project:  sess.projectVal,
			alive:    sess.isAlive(),
			juliaCmd: sess.juliaCmd,
		}
		if since := sess.busySince.Load(); since != 0 {
			info.busyFor = now.Sub(time.Unix(0, since))
		}
		if sess.logFile != nil {
			info.logFile = sess.logFile.Name()
		}
		result = append(result, info)
	}
	return result
}

func (m *SessionManager) interrupt(session, project, cwd string, graceSecs float64) (string, error) {
	key := m.key(session, project, cwd)
	m.mu.Lock()
	sess := m.sessions[key]
	m.mu.Unlock()
	if sess == nil {
		return "", fmt.Errorf("no session for %s", key)
	}
	survived, err := sess.interrupt(graceSecs)
	if err != nil {
		return "", err
	}
	if !survived {
		m.mu.Lock()
		delete(m.sessions, key)
		m.mu.Unlock()
		return fmt.Sprintf("Session %s did not survive interrupt; killed.", key), nil
	}
	return fmt.Sprintf("Session %s interrupted.", key), nil
}

func (m *SessionManager) shutdown() {
	m.mu.Lock()
	sessions := make([]*JuliaSession, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.sessions = make(map[string]*JuliaSession)
	m.mu.Unlock()

	for _, s := range sessions {
		s.kill()
	}
	os.RemoveAll(m.logDir)
}
