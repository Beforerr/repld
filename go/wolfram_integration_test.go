package main

import (
	"os"
	"os/exec"
	"path/filepath"
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

	res = repldOK(t, socketPath, cwd, "wolframscript", "-c", `Print[42]`)
	require.Equal(t, "42\nNull\n", lf(res.stdout))

	res = repldOK(t, socketPath, cwd, "wolframscript", "-c", `x = 7;`)
	require.Equal(t, "Null\n", lf(res.stdout))

	res = repldOK(t, socketPath, cwd, "wolframscript", "-c", `x + 1`)
	require.Equal(t, "8\n", lf(res.stdout))

	res = repldOK(t, socketPath, cwd, "wolframscript", "-c", `"hi"`)
	require.Equal(t, "hi\n", lf(res.stdout))

	// Messages don't imply failure: 1/0 emits Power::infy on stderr but returns
	// ComplexInfinity with exit 0.
	res = repldOK(t, socketPath, cwd, "wolframscript", "-c", `1 / 0`)
	require.Equal(t, "ComplexInfinity\n", lf(res.stdout))
	require.Contains(t, res.stderr, "Power::infy")

	// Syntax error is a genuine failure: nonzero exit, ERROR text names the
	// triggering message rather than a generic "evaluation failed".
	res = repldErr(t, socketPath, cwd, "wolframscript", "-c", `f[`)
	require.Contains(t, res.stderr, "ERROR:")
	require.Contains(t, res.stderr, "ToExpression::sntxi")

	res = repldOK(t, socketPath, cwd, "trace", "wolframscript")
	require.Contains(t, res.stdout, "ToExpression::sntxi")

	// File eval: args via Block-scoped $ScriptCommandLine, globals persist.
	script := filepath.Join(cwd, "script.wl")
	require.NoError(t, os.WriteFile(script,
		[]byte("Print[\"hi from file\"]\nPrint[StringRiffle[Rest[$ScriptCommandLine], \",\"]]\nwlFileMarker = 55\n"), 0644))

	res = repldOK(t, socketPath, cwd, "wolframscript", script, "a", "b")
	require.Contains(t, lf(res.stdout), "hi from file")
	require.Contains(t, lf(res.stdout), "a,b")

	res = repldOK(t, socketPath, cwd, "wolframscript", "-c", "wlFileMarker")
	require.Equal(t, "55\n", lf(res.stdout))
}
