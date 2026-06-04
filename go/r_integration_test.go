package main

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/Beforerr/repld/go/r"
	"github.com/stretchr/testify/require"
)

func TestRAdapter(t *testing.T) {
	if _, err := exec.LookPath(r.Adapter{}.DefaultExe()); err != nil {
		t.Skipf("%s not installed", r.Adapter{}.DefaultExe())
	}
	socketPath, stop, _ := startTestDaemon(t)
	defer stop()

	cwd := t.TempDir()
	lf := func(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }

	res := repldOK(t, socketPath, cwd, "R", "-e", `cat(21 * 2, "\n", sep = "")`)
	require.Equal(t, "42\n", lf(res.stdout))
	require.Empty(t, res.stderr)

	repldOK(t, socketPath, cwd, "R", "-e", `z <- 7`)
	res = repldOK(t, socketPath, cwd, "R", "-e", `cat(z * 3, "\n", sep = "")`)
	require.Equal(t, "21\n", lf(res.stdout))

	res = repldOK(t, socketPath, cwd, "R", "-e", `1 + 1`)
	require.Equal(t, "[1] 2\n", lf(res.stdout))

	res = repldErr(t, socketPath, cwd, "R", "-e", `stop("boom")`)
	require.Contains(t, res.stderr, "ERROR: boom")
	require.Contains(t, res.stderr, "Trace saved")

	res = repldOK(t, socketPath, cwd, "trace", "R")
	require.Contains(t, res.stdout, "simpleError")
	require.Contains(t, res.stdout, "boom")
}
