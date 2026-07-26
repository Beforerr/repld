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

func TestIdentifiesHarness(t *testing.T) {
	for _, tc := range []struct {
		ident string
		want  bool
	}{
		{"claude\x00/usr/local/bin/claude\x00", true},
		{"codex\x00/opt/homebrew/bin/codex\x00--yolo\x00", true},
		{"zsh\x00/bin/zsh\x00-c\x00repld julia -e 1\x00", false},
		{"node\x00/usr/bin/node\x00/Users/me/.claude/cli.js\x00", false},
		{"codex-wrapper\x00/usr/local/bin/codex-wrapper\x00", false},
	} {
		t.Run(tc.ident, func(t *testing.T) {
			require.Equal(t, tc.want, identifiesHarness(tc.ident))
		})
	}
}

func TestHarnessPID(t *testing.T) {
	parents := map[int]int{40: 30, 30: 20, 20: 1}
	idents := map[int]string{40: "zsh", 30: "codex", 20: "login"}
	parent := func(pid int) (int, bool) {
		ppid, ok := parents[pid]
		return ppid, ok
	}
	require.Equal(t, 30, harnessPID(40, parent, func(pid int) string { return idents[pid] }))
}
