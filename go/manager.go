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

type SessionManager struct {
	mu         sync.Mutex
	sessions   map[string]*Session
	lastErrors map[string]*evalError
	sf         singleflight.Group
	logDir     string
}

func newSessionManager() *SessionManager {
	logDir, _ := os.MkdirTemp("", "repld-logs-")
	return &SessionManager{
		sessions:   make(map[string]*Session),
		lastErrors: make(map[string]*evalError),
		logDir:     logDir,
	}
}

// key namespaces a session. A --session label is global (reusable without
// re-specifying interpreter). Otherwise it's lang + cwd + disc, where disc
// is the adapter's per-environment discriminant (Julia's --project).
func (m *SessionManager) key(lang, session, cwd, disc string) string {
	if session != "" {
		return "~" + session
	}
	route := cwd
	if lang == "julia" && disc != "" && disc != "@." {
		route = disc
		if !strings.HasPrefix(route, "@") && !filepath.IsAbs(route) {
			route = filepath.Join(cwd, route)
		}
		route = filepath.Clean(route)
	}
	return lang + "\x00" + route + "\x00" + disc
}

// keyLabel is the human label for a key: a "~label" session label, else the cwd
// (the lang prefix and project discriminant are shown separately).
func keyLabel(key string) string {
	if !strings.Contains(key, "\x00") {
		return key
	}
	return strings.SplitN(key, "\x00", 3)[1]
}

func (m *SessionManager) openLogFile(key string) *os.File {
	safe := strings.NewReplacer("/", "_", "\\", "_", "\x00", "-").Replace(strings.Trim(key, "/~"))
	if safe == "" {
		safe = "default"
	}
	f, _ := os.OpenFile(filepath.Join(m.logDir, safe+".log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	return f
}

// forwarded args apply only when a session is first created;
// a live session for the key is reused as-is.
func (m *SessionManager) getOrCreate(lang, cwd, session, exe string, fwd []string) (*Session, error) {
	lc, known := langs[lang]
	disc := ""
	if known {
		disc = lc.adapter.SessionKey(exe, fwd)
	}
	key := m.key(lang, session, cwd, disc)

	m.mu.Lock()
	sess := m.sessions[key]
	m.mu.Unlock()
	if sess != nil && sess.isAlive() {
		return sess, nil
	}

	v, err, _ := m.sf.Do(key, func() (any, error) {
		m.mu.Lock()
		sess := m.sessions[key]
		m.mu.Unlock()
		if sess != nil && sess.isAlive() {
			return sess, nil
		}
		if !known {
			return nil, fmt.Errorf("unknown language %q; pass an interpreter or --lang", lang)
		}
		if sess != nil {
			sess.kill()
			m.mu.Lock()
			delete(m.sessions, key)
			m.mu.Unlock()
		}

		sess = newSession(lc.adapter, newSentinel(), fwd, m.openLogFile(key))
		sess.lang = lang
		if err := sess.start(exe, cwd); err != nil {
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
	return v.(*Session), nil
}

func (m *SessionManager) remove(lang, session, cwd, disc string) {
	key := m.key(lang, session, cwd, disc)
	m.mu.Lock()
	delete(m.sessions, key)
	m.mu.Unlock()
}

func (m *SessionManager) restart(lang, session, cwd, disc string) {
	key := m.key(lang, session, cwd, disc)
	m.mu.Lock()
	sess := m.sessions[key]
	delete(m.sessions, key)
	delete(m.lastErrors, key)
	m.mu.Unlock()
	if sess != nil {
		sess.kill()
	}
}

func (m *SessionManager) hasLiveSession(lang, session, cwd, disc string) bool {
	key := m.key(lang, session, cwd, disc)
	m.mu.Lock()
	defer m.mu.Unlock()
	sess := m.sessions[key]
	return sess != nil && sess.isAlive()
}

func (m *SessionManager) recordError(lang, session, cwd, disc string, err *evalError) {
	key := m.key(lang, session, cwd, disc)
	m.mu.Lock()
	m.lastErrors[key] = err
	m.mu.Unlock()
}

func (m *SessionManager) lastError(lang, session, cwd, disc string) *evalError {
	key := m.key(lang, session, cwd, disc)
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastErrors[key]
}

type sessionInfo struct {
	lang    string
	label   string // session label or cwd
	project string // environment discriminant (Julia --project); "" if none
	alive   bool
	args    []string
	logFile string
	busyFor time.Duration // 0 when idle
}

func (m *SessionManager) list() []sessionInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	result := make([]sessionInfo, 0, len(m.sessions))
	for key, sess := range m.sessions {
		info := sessionInfo{
			lang:  sess.lang,
			label: keyLabel(key),
			alive: sess.isAlive(),
			args:  sess.fwd,
		}
		if sess.lang == "julia" {
			info.project = sess.adapter.SessionKey("", sess.fwd)
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

func (m *SessionManager) interrupt(lang, session, cwd, disc string, graceSecs float64) (string, error) {
	key := m.key(lang, session, cwd, disc)
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
	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.sessions = make(map[string]*Session)
	m.mu.Unlock()

	for _, s := range sessions {
		s.kill()
	}
	os.RemoveAll(m.logDir)
}
