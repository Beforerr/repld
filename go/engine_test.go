package main

import (
	"net"
	"testing"
	"time"

	"github.com/Beforerr/repld/go/python"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestAcceptControl(t *testing.T) {
	const token = "tok-correct"

	t.Run("valid wins over earlier stray", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		addr := ln.Addr().String()

		stray, err := net.Dial("tcp", addr)
		require.NoError(t, err)
		defer stray.Close()

		go func() {
			time.Sleep(50 * time.Millisecond)
			c, derr := net.Dial("tcp", addr)
			require.NoError(t, derr)
			c.Write([]byte(token + "\n"))
			// Runtime side writes the first control frame after authenticating.
			c.Write([]byte("OK\n"))
		}()

		conn, br := acceptControl(ln, token, 2.0)
		require.NotNil(t, conn)
		require.NotNil(t, br)
		defer conn.Close()
		line, err := br.ReadString('\n')
		require.NoError(t, err)
		require.Equal(t, "OK\n", line)
	})

	t.Run("only wrong tokens times out to nil", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		addr := ln.Addr().String()

		for i := 0; i < 3; i++ {
			c, derr := net.Dial("tcp", addr)
			require.NoError(t, derr)
			c.Write([]byte("wrong\n"))
			defer c.Close()
		}

		start := time.Now()
		conn, br := acceptControl(ln, token, 0.5)
		require.Nil(t, conn)
		require.Nil(t, br)
		require.Less(t, time.Since(start), 2*time.Second)
	})
}

func TestHandleRequest_Ping(t *testing.T) {
	resp := handleRequest(newTestState(), protocolRequest{Action: "ping"})
	require.Equal(t, "pong", resp.Output)
}

func TestHandleRequest_SessionsEmpty(t *testing.T) {
	resp := handleRequest(newTestState(), protocolRequest{Action: "sessions"})
	require.Equal(t, "No active sessions.", resp.Output)
}

func TestHandleRequest_UnknownAction(t *testing.T) {
	resp := handleRequest(newTestState(), protocolRequest{Action: "bogus"})
	require.NotEmpty(t, resp.Error)
}

func TestFormatErrorTraceHintIncludesSessionID(t *testing.T) {
	err := &evalError{short: "short", smart: "smart\n", full: "full"}
	require.Equal(t, "smart\nTrace saved: `repld trace klmnopqr`.", formatError(err, "smart", "klmnopqr"))
	require.Equal(t, "short\n\nTrace saved: `repld trace klmnopqr`.", formatError(err, "short", "klmnopqr"))
	require.Equal(t, "full", formatError(err, "full", "klmnopqr"))
}

func TestHandleRequest_Stop(t *testing.T) {
	state := newTestState()
	resp := handleRequest(state, protocolRequest{Action: "stop"})
	require.Equal(t, "Daemon stopping.", resp.Output)
	select {
	case <-state.stopCh:
	default:
		require.Fail(t, "stopCh not closed after stop action")
	}
}

func TestHandleRequest_SessionsList(t *testing.T) {
	state := newTestState()
	// A Julia cwd session with startup args; a global labeled session; a dead
	// python dir session. The key is lang-prefixed; --session labels are global.
	jl := newSession("julia", "s", []string{"--project=/env"}, nil)
	jl.id = "kqzmkqzm"
	named := newSession("julia", "s", nil, nil)
	py := newSession("python", "s", nil, nil)
	py.dead.Store(true)
	state.manager.sessions[sessionKey{lang: "julia", cwd: "/work", exe: "julia"}] = jl
	state.manager.sessions[sessionKey{label: "scratch"}] = named
	state.manager.sessions[sessionKey{lang: "python", cwd: "/work", exe: "python3"}] = py

	resp := handleRequest(state, protocolRequest{Action: "sessions"})
	require.Empty(t, resp.Error)
	require.Equal(t, `- {id: kqzmkqzm, lang: julia, dir: /work, args: [--project=/env]}
- {lang: julia, session: scratch}
- {lang: python, dir: /work, status: dead}
`, resp.Output)
}

func TestHandleRequest_SessionsParseableYAML(t *testing.T) {
	state := newTestState()
	jl := newSession("julia", "s", []string{"--project=/env"}, nil)
	jl.id = "kqzmkqzm"
	named := newSession("julia", "s", nil, nil)
	state.manager.sessions[sessionKey{lang: "julia", cwd: "/work", exe: "julia"}] = jl
	state.manager.sessions[sessionKey{label: "scratch"}] = named

	resp := handleRequest(state, protocolRequest{Action: "sessions"})
	require.Empty(t, resp.Error)

	var items []sessionInfo
	require.NoError(t, yaml.Unmarshal([]byte(resp.Output), &items))
	require.Len(t, items, 2)
	require.Equal(t, "kqzmkqzm", items[0].ID)
	require.Equal(t, "julia", items[0].Lang)
	require.Equal(t, "scratch", items[1].Session)
}

func TestInterruptUnknownSession(t *testing.T) {
	resp := handleRequest(newTestState(), protocolRequest{Action: "interrupt", Session: "nope", Cwd: t.TempDir()})
	require.NotEmpty(t, resp.Error)
	require.Contains(t, resp.Error, "no session")
}

func TestCloseUnknownSession(t *testing.T) {
	resp := handleRequest(newTestState(), protocolRequest{Action: "close", Session: "nope", Cwd: t.TempDir()})
	require.NotEmpty(t, resp.Error)
	require.Contains(t, resp.Error, "no session")
}

func TestCloseSession(t *testing.T) {
	state := newTestState()
	sess := newSession("julia", "s", nil, nil)
	state.manager.sessions[sessionKey{label: "scratch"}] = sess

	resp := handleRequest(state, protocolRequest{Action: "close", Session: "scratch", Cwd: t.TempDir()})
	require.Empty(t, resp.Error)
	require.Contains(t, resp.Output, "closed")
	require.Empty(t, state.manager.sessions)
	require.False(t, sess.isAlive())
}

func TestCloseSessionByID(t *testing.T) {
	state := newTestState()
	sess := newSession("julia", "s", nil, nil)
	sess.id = "kqzm"
	state.manager.sessions[sessionKey{lang: "julia", cwd: "/work", exe: "julia"}] = sess

	// Unique prefix resolves regardless of cwd.
	resp := handleRequest(state, protocolRequest{Action: "close", ID: "kq", Cwd: t.TempDir()})
	require.Empty(t, resp.Error)
	require.Contains(t, resp.Output, "closed")
	require.Empty(t, state.manager.sessions)

	resp = handleRequest(state, protocolRequest{Action: "close", ID: "kq", Cwd: t.TempDir()})
	require.Contains(t, resp.Error, `no session with id "kq"`)
}

func TestKeyForIDPrefix(t *testing.T) {
	m := newSessionManager()
	defer m.shutdown()
	a := newSession("julia", "s", nil, nil)
	a.id = "kqzm"
	b := newSession("julia", "s", nil, nil)
	b.id = "kxyz"
	m.sessions[sessionKey{lang: "julia", cwd: "/a", exe: "julia"}] = a
	m.sessions[sessionKey{lang: "julia", cwd: "/b", exe: "julia"}] = b

	key, ok, err := m.keyForID("kq")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, sessionKey{lang: "julia", cwd: "/a", exe: "julia"}, key)

	_, _, err = m.keyForID("k") // shared prefix → ambiguous
	require.Error(t, err)
	require.Contains(t, err.Error(), "ambiguous")

	_, ok, err = m.keyForID("zz") // no match
	require.NoError(t, err)
	require.False(t, ok)
}

func TestSessionManagerKey(t *testing.T) {
	m := newSessionManager()
	defer m.shutdown()

	// key = language + cwd + interpreter; labels are global.
	require.Equal(t, sessionKey{lang: "julia", cwd: "/w", exe: "julia"}, m.key("julia", "", "/w", ""))
	require.Equal(t, sessionKey{lang: "python", cwd: "/w", exe: python.Adapter{}.DefaultExe()}, m.key("python", "", "/w", ""))
	require.NotEqual(t, m.key("julia", "", "/w", "julia"), m.key("python", "", "/w", "python3"))
	require.NotEqual(t, m.key("julia", "", "/w", "julia"), m.key("julia", "", "/w", "/opt/julia"))
	require.NotEqual(t, m.key("julia", "", "/a", "julia"), m.key("julia", "", "/b", "julia"))
	// a --session label is global: same key regardless of language/interpreter.
	require.Equal(t, sessionKey{label: "scratch"}, m.key("julia", "scratch", "/w", "julia"))
	require.Equal(t, sessionKey{label: "scratch"}, m.key("python", "scratch", "/other", ""))
}
