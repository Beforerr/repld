package main

import (
	"crypto/rand"
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
	sessions   map[sessionKey]*Session
	lastErrors map[sessionKey]*evalError
	sf         singleflight.Group
	logDir     string
}

type sessionKey struct {
	label string
	lang  string
	route string
	disc  string
}

func (k sessionKey) String() string {
	if k.label != "" {
		return "session:" + k.label
	}
	if k.disc == "" {
		return k.lang + ":" + k.route
	}
	return k.lang + ":" + k.route + ":" + k.disc
}

func (k sessionKey) logName() string {
	if k.label != "" {
		return "session-" + safeLogComponent(k.label)
	}
	parts := []string{safeLogComponent(k.lang), safeLogComponent(k.route)}
	if k.disc != "" {
		parts = append(parts, safeLogComponent(k.disc))
	}
	return strings.Join(parts, "-")
}

func safeLogComponent(s string) string {
	s = strings.NewReplacer("/", "_", "\\", "_", ":", "_", "\"", "_").Replace(strings.Trim(s, "/"))
	if s == "" {
		return "default"
	}
	return s
}

func newSessionManager() *SessionManager {
	logDir, _ := os.MkdirTemp("", "repld-logs-")
	return &SessionManager{
		sessions:   make(map[sessionKey]*Session),
		lastErrors: make(map[sessionKey]*evalError),
		logDir:     logDir,
	}
}

// key namespaces a session. A --session label is global (reusable without
// re-specifying interpreter). Otherwise route is cwd or the adapter's
// per-environment location (Julia's --project).
func (m *SessionManager) key(lang, session, cwd, disc string) sessionKey {
	if session != "" {
		return sessionKey{label: session}
	}
	route := cwd
	if lang == "julia" && disc != "" && disc != "@." {
		route = disc
		if !strings.HasPrefix(route, "@") && !filepath.IsAbs(route) {
			route = filepath.Join(cwd, route)
		}
		route = filepath.Clean(route)
	}
	return sessionKey{lang: lang, route: route, disc: disc}
}

// from k-z : IDs never look like exe names, paths
const idAlphabet = "klmnopqrstuvwxyz"
const idLen = 8

func (m *SessionManager) uniqueIDLocked() string {
	for {
		b := make([]byte, idLen)
		rand.Read(b)
		id := make([]byte, idLen)
		for i, c := range b {
			id[i] = idAlphabet[int(c)%len(idAlphabet)]
		}
		inUse := false
		for _, sess := range m.sessions {
			if sess.id == string(id) {
				inUse = true
				break
			}
		}
		if !inUse {
			return string(id)
		}
	}
}

func (m *SessionManager) keyForID(prefix string) (sessionKey, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var match sessionKey
	found := false
	for key, sess := range m.sessions {
		if sess.id != "" && strings.HasPrefix(sess.id, prefix) {
			if found {
				return sessionKey{}, false, fmt.Errorf("session id %q is ambiguous", prefix)
			}
			match = key
			found = true
		}
	}
	return match, found, nil
}

func (m *SessionManager) targetKey(id, lang, session, cwd, disc string) (sessionKey, error) {
	if id != "" {
		key, ok, err := m.keyForID(id)
		if err != nil {
			return sessionKey{}, err
		}
		if !ok {
			return sessionKey{}, fmt.Errorf("no session with id %q", id)
		}
		return key, nil
	}
	return m.key(lang, session, cwd, disc), nil
}

func (m *SessionManager) openLogFile(key sessionKey) *os.File {
	f, _ := os.OpenFile(filepath.Join(m.logDir, key.logName()+".log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	return f
}

// forwarded args apply only when a session is first created;
// a live session for the key is reused as-is.
func (m *SessionManager) getOrCreate(lang, cwd, session, exe string, fwd []string, ownerPID int, ownerStart int64) (*Session, error) {
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

	v, err, _ := m.sf.Do(key.String(), func() (any, error) {
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

		sess = newSession(lang, newSentinel(), fwd, m.openLogFile(key))
		if err := sess.start(exe, cwd); err != nil {
			return nil, err
		}

		m.mu.Lock()
		sess.id = m.uniqueIDLocked()
		sess.ownerPID, sess.ownerStart = ownerPID, ownerStart
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

func (m *SessionManager) close(key sessionKey) (string, error) {
	m.mu.Lock()
	sess := m.sessions[key]
	delete(m.sessions, key)
	delete(m.lastErrors, key)
	m.mu.Unlock()
	if sess == nil {
		return "", fmt.Errorf("no session for %s", key)
	}
	sess.kill()
	return fmt.Sprintf("Session %s closed.", key), nil
}

func (m *SessionManager) free(key sessionKey) (string, error) {
	m.mu.Lock()
	sess := m.sessions[key]
	if sess != nil {
		sess.ownerPID, sess.ownerStart = 0, 0
	}
	m.mu.Unlock()
	if sess == nil {
		return "", fmt.Errorf("no session for %s", key)
	}
	return fmt.Sprintf("Session %s freed; it will not auto-close.", key), nil
}

func (m *SessionManager) reapDeadOwners() {
	m.mu.Lock()
	var dead []sessionKey
	for key, sess := range m.sessions {
		if ownerDead(sess.ownerPID, sess.ownerStart) {
			dead = append(dead, key)
		}
	}
	m.mu.Unlock()
	for _, key := range dead {
		m.close(key)
	}
}

func (m *SessionManager) reapLoop(stop <-chan struct{}) {
	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			m.reapDeadOwners()
		}
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

func (m *SessionManager) lastError(key sessionKey) *evalError {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastErrors[key]
}

// sessionInfo is both the internal listing row and the YAML output shape.
type sessionInfo struct {
	ID      string   `yaml:"id,omitempty"`
	Lang    string   `yaml:"lang"`
	Dir     string   `yaml:"dir,omitempty"`     // working directory; "" for named sessions
	Session string   `yaml:"session,omitempty"` // named-session label; "" for cwd-keyed sessions
	Status  string   `yaml:"status,omitempty"`  // "dead" or "busy"; "" when idle and alive
	Busy    float64  `yaml:"busy,omitempty"`    // seconds in-flight; set only while busy
	Owner   int      `yaml:"owner,omitempty"`   // owner pid whose exit auto-closes the session; 0 = pinned
	Args    []string `yaml:"args,omitempty"`    // effective launch args (Julia's implicit --project is made explicit)
	Log     string   `yaml:"log,omitempty"`
}

func hasProjectFlag(args []string) bool {
	for _, a := range args {
		if a == "--project" || strings.HasPrefix(a, "--project=") {
			return true
		}
	}
	return false
}

func (m *SessionManager) list() []sessionInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	result := make([]sessionInfo, 0, len(m.sessions))
	for key, sess := range m.sessions {
		info := sessionInfo{
			ID:    sess.id,
			Lang:  sess.lang,
			Args:  sess.fwd,
			Owner: sess.ownerPID,
		}
		if key.label != "" {
			info.Session = key.label
		} else {
			info.Dir = key.route
		}
		// Surface Julia's implicit project so args reflect the real launch.
		if sess.lang == "julia" && !hasProjectFlag(sess.fwd) {
			info.Args = append([]string{"--project=" + sess.adapter.SessionKey("", sess.fwd)}, sess.fwd...)
		}
		if !sess.isAlive() {
			info.Status = "dead"
		} else if since := sess.busySince.Load(); since != 0 {
			info.Status = "busy"
			info.Busy = now.Sub(time.Unix(0, since)).Seconds()
		}
		if sess.logFile != nil {
			info.Log = sess.logFile.Name()
		}
		result = append(result, info)
	}
	return result
}

func (m *SessionManager) interrupt(key sessionKey, graceSecs float64) (string, error) {
	m.mu.Lock()
	sess := m.sessions[key]
	m.mu.Unlock()
	if sess == nil {
		return "", fmt.Errorf("no session for %s", key)
	}
	survived, idle, err := sess.interrupt(graceSecs)
	if err != nil {
		return "", err
	}
	if idle {
		return fmt.Sprintf("Session %s idle; nothing to interrupt.", key), nil
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
	m.sessions = make(map[sessionKey]*Session)
	m.mu.Unlock()

	for _, s := range sessions {
		s.kill()
	}
	os.RemoveAll(m.logDir)
}
