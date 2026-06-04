package main

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/Beforerr/repld/go/wolfram"
	"github.com/stretchr/testify/require"
)

func TestWolframAdapter(t *testing.T) {
	if _, err := exec.LookPath(wolfram.Adapter{}.DefaultExe()); err != nil {
		t.Skipf("%s not installed", wolfram.Adapter{}.DefaultExe())
	}
	socketPath, stop, _ := startTestDaemon(t)
	defer stop()

	cwd := t.TempDir()
	lf := func(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }

	res := repldOK(t, socketPath, cwd, "wolframscript", "-c", `21 * 2`)
	require.Equal(t, "42\n", lf(res.stdout))
	require.Empty(t, res.stderr)

	repldOK(t, socketPath, cwd, "wolframscript", "-c", `z = 7`)
	res = repldOK(t, socketPath, cwd, "wolframscript", "-c", `z * 3`)
	require.Equal(t, "21\n", lf(res.stdout))

	res = repldErr(t, socketPath, cwd, "wolframscript", "-c", `1 / 0`)
	require.Contains(t, res.stderr, "ERROR:")

	res = repldOK(t, socketPath, cwd, "trace", "wolframscript")
	require.Contains(t, res.stdout, "ERROR:")
}
