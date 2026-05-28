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
	mu     sync.Mutex

	dead       atomic.Bool
	busySince  atomic.Int64 // UnixNano of current call start; 0 when idle
	logFile    *os.File
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

	// Merge stdout+stderr into a single pipe (mirrors Python's stderr=subprocess.STDOUT)
	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		pw.Close()
		pr.Close()
		return err
	}
	pw.Close() // parent only reads; close the write end so EOF propagates when process exits

	s.proc = cmd
	s.stdin = stdin
	s.stdout = bufio.NewReaderSize(pr, 64*1024*1024)

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

// executeRaw writes code and a sentinel command to stdin, then reads stdout
// until the sentinel marker is observed. If onLine is non-nil, it is invoked
// for each user-printed chunk as it arrives. Lines inside the runtime's
// error block are buffered (for parseJuliaError) but not streamed.
//
// The sentinel and error start-marker are detected via HasSuffix so that
// user code that prints without a trailing newline still terminates cleanly
// (the marker rides on the tail of the same line). Sentinel collision risk
// is negligible: the marker contains 16 random hex bytes per session.
func (s *JuliaSession) executeRaw(code string, onLine func(string), timeoutSecs float64) (string, error) {
	sentinelCmd := fmt.Sprintf(
		"flush(stderr); println(stdout, \"%s\"); flush(stdout)\n",
		s.sentinel,
	)
	if _, err := io.WriteString(s.stdin, code+"\n"+sentinelCmd); err != nil {
		return "", err
	}

	startMarker := s.errorStartMarker()
	endMarker := s.errorEndMarker()
	ch := make(chan readResult, 1)
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
					ch <- readResult{buf.String(), fmt.Errorf("Julia process died during execution.\nOutput before death:\n%s", buf.String())}
					return
				}
				continue
			}

			if strings.HasSuffix(raw, s.sentinel) {
				if prefix := strings.TrimSuffix(raw, s.sentinel); prefix != "" {
					buf.WriteString(prefix)
					if onLine != nil {
						onLine(prefix)
					}
				}
				ch <- readResult{buf.String(), nil}
				return
			}
			if strings.HasSuffix(raw, startMarker) {
				if prefix := strings.TrimSuffix(raw, startMarker); prefix != "" {
					buf.WriteString(prefix)
					if onLine != nil {
						onLine(prefix)
					}
				}
				buf.WriteString(startMarker + "\n")
				inErrorBlock = true
				continue
			}

			if err != nil {
				s.dead.Store(true)
				ch <- readResult{buf.String(), fmt.Errorf("Julia process died during execution.\nOutput before death:\n%s", buf.String())}
				return
			}

			buf.WriteString(line)
			if onLine != nil {
				onLine(line)
			}
		}
	}()

	if timeoutSecs <= 0 {
		r := <-ch
		return r.output, r.err
	}

	timer := time.NewTimer(time.Duration(float64(time.Second) * timeoutSecs))
	defer timer.Stop()

	select {
	case r := <-ch:
		return r.output, r.err
	case <-timer.C:
		s.proc.Process.Kill()
		s.proc.Wait()
		s.dead.Store(true)
		r := <-ch // goroutine unblocks on EOF after kill
		msg := fmt.Sprintf("Execution timed out after %vs. Session killed; it will restart on next call.", timeoutSecs)
		if r.output != "" {
			msg += "\n\nOutput before timeout:\n" + r.output
		}
		return "", fmt.Errorf("%s", msg)
	}
}

// execute runs user code via the JuliaClientRuntime wrapper so errors are
// caught and delivered as a structured *juliaEvalError. If onLine is non-nil,
// it is invoked for each chunk of user output as it arrives.
func (s *JuliaSession) execute(code string, printResult bool, onLine func(string)) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.dead.Load() {
		return "", fmt.Errorf("Julia session has died unexpectedly")
	}

	hexCode := hex.EncodeToString([]byte(code))
	wrapped := fmt.Sprintf(`Main.JuliaClientRuntime.run("%s", %t, "%s", "%s")`,
		hexCode, printResult, s.errorStartMarker(), s.errorEndMarker(),
	)

	if s.logFile != nil {
		fmt.Fprintf(s.logFile, "[%s] julia> %s\n", time.Now().Format("15:04:05"), code)
	}

	s.busySince.Store(time.Now().UnixNano())
	defer s.busySince.Store(0)

	output, err := s.executeRaw(wrapped, onLine, 0)
	if err != nil {
		return "", err
	}
	output, juliaErr := s.parseJuliaError(output)
	if s.logFile != nil && output != "" {
		fmt.Fprintf(s.logFile, "%s\n\n", output)
	}
	if juliaErr != nil {
		if s.logFile != nil {
			fmt.Fprintf(s.logFile, "%s\n\n", juliaErr.full)
		}
		return output, juliaErr
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
