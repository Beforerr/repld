package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Beforerr/repld/go/python"
	"github.com/stretchr/testify/require"
)

func pythonTestDaemon(t *testing.T) (socketPath, pythonExe string) {
	t.Helper()
	pythonExe, err := exec.LookPath(python.Adapter{}.DefaultExe())
	if err != nil {
		t.Skipf("%s not installed", python.Adapter{}.DefaultExe())
	}
	socketPath, stop, _ := startTestDaemon(t)
	t.Cleanup(stop)
	return socketPath, pythonExe
}

func TestPythonAdapter(t *testing.T) {
	socketPath, _ := pythonTestDaemon(t)

	cwd, err := os.Getwd()
	require.NoError(t, err)

	res := repldOK(t, socketPath, cwd, "python3", "-c", `print(21 * 2)`)
	require.Equal(t, "42\n", res.stdout)
	require.Empty(t, res.stderr)

	repldOK(t, socketPath, cwd, "python3", "-c", `z = 7`)
	res = repldOK(t, socketPath, cwd, "python3", "-c", `print(z * 3)`)
	require.Equal(t, "21\n", res.stdout)

	res = repldErr(t, socketPath, cwd, "python3", "-c", `raise ValueError("boom")`)
	require.Contains(t, res.stderr, "boom")
}

func TestPythonSessionsAreKeyedByInterpreter(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinked executable test is unix-only")
	}
	socketPath, pythonExe := pythonTestDaemon(t)

	tmp := t.TempDir()
	pyA := filepath.Join(tmp, "python-a")
	pyB := filepath.Join(tmp, "python-b")
	wrapper := []byte(fmt.Sprintf("#!/bin/sh\nexec %q \"$@\"\n", pythonExe))
	require.NoError(t, os.WriteFile(pyA, wrapper, 0755))
	require.NoError(t, os.WriteFile(pyB, wrapper, 0755))
	cwd := t.TempDir()

	send := func(exe, code string) cliResult {
		return repldOK(t, socketPath, cwd, exe, "-c", code)
	}
	send(pyA, `x = "a"`)
	send(pyB, `x = "b"`)
	require.Equal(t, "a\n", send(pyA, `print(x)`).stdout)
	require.Equal(t, "b\n", send(pyB, `print(x)`).stdout)
}

func TestPythonFileRunUsesMainAndPersistsState(t *testing.T) {
	socketPath, _ := pythonTestDaemon(t)

	cwd := t.TempDir()
	script := filepath.Join(cwd, "script.py")
	require.NoError(t, os.WriteFile(script, []byte(`if __name__ == "__main__":
    marker = "ran-main"
`), 0644))

	// Forward the script as the program: it runs at launch as __main__, and its
	// state is visible to later evals in the same warm session.
	repldOK(t, socketPath, cwd, python.Adapter{}.DefaultExe(), script)

	check := repldOK(t, socketPath, cwd, python.Adapter{}.DefaultExe(), "-c", "print(marker)")
	require.Equal(t, "ran-main\n", check.stdout)
}

func TestCLIPythonEvalDoesNotLeakInteractivePrompts(t *testing.T) {
	socketPath, _ := pythonTestDaemon(t)

	res := repldOK(t, socketPath, "", "python3", "-c", "x=1")
	require.Empty(t, res.stdout)
	require.Empty(t, res.stderr)
}

func TestCLIPythonMissingScriptBehavesLikeInteractiveInterpreter(t *testing.T) {
	socketPath, _ := pythonTestDaemon(t)

	res := repldOK(t, socketPath, "", "python3", "x=1")
	require.Contains(t, res.stderr, "can't open file")
	require.NotContains(t, res.stderr, ">>>")
	require.NotContains(t, res.stderr, "repld: persistent REPL daemon")
}

func TestPythonTracebackLineNumbers(t *testing.T) {
	socketPath, _ := pythonTestDaemon(t)

	cwd, err := os.Getwd()
	require.NoError(t, err)

	// Error on line 3; traceback must say line 3.
	resp := sendRequest(t, socketPath, protocolRequest{
		Action: "eval",
		Lang:   "python",
		Code:   "x = 1\ny = 2\nraise ValueError('line3error')",
		Cwd:    cwd,
	})
	require.Contains(t, resp.Error, "line3error")
	require.Contains(t, resp.Error, "line 3")
}
