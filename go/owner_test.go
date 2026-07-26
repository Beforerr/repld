package main

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOwnerLease(t *testing.T) {
	c := exec.Command("sleep", "300")
	require.NoError(t, c.Start())
	t.Cleanup(func() {
		_ = c.Process.Kill()
		_ = c.Wait()
	})
	start, alive := procInfo(c.Process.Pid)
	require.True(t, alive)

	m := newSessionManager()
	defer m.shutdown()

	owned := sessionKey{label: "owned"}
	freed := sessionKey{label: "freed"}
	for _, key := range []sessionKey{owned, freed} {
		sess := newSession("julia", "s", nil, nil)
		sess.ownerPID, sess.ownerStart = c.Process.Pid, start
		m.sessions[key] = sess
	}

	m.reapDeadOwners()
	require.Contains(t, m.sessions, owned)
	_, err := m.free(freed)
	require.NoError(t, err)

	require.NoError(t, c.Process.Kill())
	_ = c.Wait()
	m.reapDeadOwners()
	require.NotContains(t, m.sessions, owned)
	require.Contains(t, m.sessions, freed)
}
