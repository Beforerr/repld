package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

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

// juliaArgs apply only when a session is first created; a live session for the
// key is reused as-is regardless (use --fresh to rebuild with new args).
func (m *SessionManager) getOrCreate(cwd, project, session string, juliaArgs []string) (*JuliaSession, error) {
	key := m.key(session, project, cwd)

	// Fast path: return existing live session without singleflight overhead.
	m.mu.Lock()
	sess := m.sessions[key]
	m.mu.Unlock()
	if sess != nil && sess.isAlive() {
		return sess, nil
	}

	// Slow path: deduplicate concurrent creation for the same key.
	v, err, _ := m.sf.Do(key, func() (any, error) {
		m.mu.Lock()
		sess := m.sessions[key]
		m.mu.Unlock()
		if sess != nil && sess.isAlive() {
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
		sess = newJuliaSession(projectVal, newSentinel(), juliaArgs, m.openLogFile(key))
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
	key       string
	project   string
	alive     bool
	juliaArgs []string
	logFile   string
	busyFor   time.Duration // 0 when idle
}

func (m *SessionManager) list() []sessionInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	result := make([]sessionInfo, 0, len(m.sessions))
	for key, sess := range m.sessions {
		info := sessionInfo{
			key:       key,
			project:   sess.projectVal,
			alive:     sess.isAlive(),
			juliaArgs: sess.juliaArgs,
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
