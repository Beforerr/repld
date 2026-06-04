package main

import (
	"path/filepath"
	"testing"

	"github.com/Beforerr/repld/go/julia"
	"github.com/Beforerr/repld/go/python"
	"github.com/stretchr/testify/require"
)

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
	// A julia dir session pinned to a project; a global labeled session; a dead
	// python dir session. The key is lang-prefixed; --session labels are global.
	jl := newSession(julia.Adapter{}, "s", []string{"--project=/env"}, nil)
	jl.lang = "julia"
	named := newSession(julia.Adapter{}, "s", nil, nil)
	named.lang = "julia"
	py := newSession(python.Adapter{}, "s", nil, nil)
	py.lang = "python"
	py.dead.Store(true)
	state.manager.sessions["julia\x00/work\x00/env"] = jl
	state.manager.sessions["~scratch"] = named
	state.manager.sessions["python\x00/work\x00"] = py

	resp := handleRequest(state, protocolRequest{Action: "sessions"})
	require.Empty(t, resp.Error)
	require.Equal(t, `Active sessions:
  [julia] dir /work project=/env args=--project=/env
  [julia] session scratch project=@.
  [python] dir /work status=dead`, resp.Output)
}

func TestInterruptUnknownSession(t *testing.T) {
	resp := handleRequest(newTestState(), protocolRequest{Action: "interrupt", Session: "nope", Cwd: t.TempDir()})
	require.NotEmpty(t, resp.Error)
	require.Contains(t, resp.Error, "no session")
}

func TestSessionManagerKey(t *testing.T) {
	m := newSessionManager()
	defer m.shutdown()

	// key = lang + cwd + discriminant (project); label keys are global.
	require.Equal(t, "julia\x00/w\x00@.", m.key("julia", "", "/w", "@."))
	require.Equal(t, "python\x00/w\x00", m.key("python", "", "/w", ""))
	// same dir, distinct by language or by project → distinct sessions.
	require.NotEqual(t, m.key("julia", "", "/w", "@."), m.key("python", "", "/w", ""))
	require.NotEqual(t, m.key("julia", "", "/w", "@."), m.key("julia", "", "/w", "/env"))
	absProject := filepath.Join(t.TempDir(), "env")
	require.Equal(t, m.key("julia", "", "/a", absProject), m.key("julia", "", "/b", absProject))
	require.NotEqual(t, m.key("julia", "", "/a", "@."), m.key("julia", "", "/b", "@."))
	// a --session label is global: same key regardless of language/project.
	require.Equal(t, "~scratch", m.key("julia", "scratch", "/w", "@."))
	require.Equal(t, "~scratch", m.key("python", "scratch", "/other", ""))
}
