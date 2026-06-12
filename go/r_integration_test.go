package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Beforerr/repld/go/r"
	"github.com/stretchr/testify/require"
)

func TestRAdapter(t *testing.T) {
	if _, err := exec.LookPath(r.Adapter{}.DefaultExe()); err != nil {
		t.Skipf("%s not installed", r.Adapter{}.DefaultExe())
	}
	socketPath := sharedDaemon(t)

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

func TestRFileEvalArgsAndState(t *testing.T) {
	if _, err := exec.LookPath(r.Adapter{}.DefaultExe()); err != nil {
		t.Skipf("%s not installed", r.Adapter{}.DefaultExe())
	}
	socketPath := sharedDaemon(t)

	cwd := t.TempDir()
	lf := func(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }
	script := filepath.Join(cwd, "script.R")
	require.NoError(t, os.WriteFile(script,
		[]byte("cat(commandArgs(trailingOnly=TRUE), \"\\n\")\nr_marker <<- 33\n"), 0644))

	res := repldOK(t, socketPath, cwd, "R", script, "a", "b")
	require.Equal(t, "a b \n", lf(res.stdout))
	res = repldOK(t, socketPath, cwd, "R", "-e", "cat(r_marker)")
	require.Equal(t, "33", lf(res.stdout))
	res = repldOK(t, socketPath, cwd, "R", "-e",
		`cat(exists("commandArgs", envir=globalenv(), inherits=FALSE))`)
	require.Equal(t, "FALSE", lf(res.stdout))
}

func TestRInterruptSurvives(t *testing.T) {
	if _, err := exec.LookPath(r.Adapter{}.DefaultExe()); err != nil {
		t.Skipf("%s not installed", r.Adapter{}.DefaultExe())
	}
	socketPath := sharedDaemon(t)

	cwd, err := os.Getwd()
	require.NoError(t, err)

	// Set state that must survive interrupt.
	repldOK(t, socketPath, cwd, "--session", "rirq", "R", "-e", "x <- 1")

	cmd := exec.Command(os.Args[0], "--socket", socketPath, "--session", "rirq", "R", "-e", "Sys.sleep(60)")
	cmd.Env = append(os.Environ(), "TEST_CLI=1")
	cmd.Dir = cwd
	var sleepStdout, sleepStderr bytes.Buffer
	cmd.Stdout = &sleepStdout
	cmd.Stderr = &sleepStderr
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	deadline := time.Now().Add(10 * time.Second)
	var sessOut string
	for time.Now().Before(deadline) {
		sessOut = repldOK(t, socketPath, cwd, "sessions").stdout
		if strings.Contains(sessOut, "busy=") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.Contains(t, sessOut, "busy=", "sessions listing should show busy= while a call is in flight")

	irq := repldOK(t, socketPath, cwd, "interrupt", "--session", "rirq")
	require.Contains(t, irq.stdout, "interrupted", "R must survive SIGINT, got: %q", irq.stdout)
	require.NotContains(t, irq.stdout, "killed")

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		require.Fail(t, "interrupted call did not return")
	}

	after := repldOK(t, socketPath, cwd, "--session", "rirq", "R", "-e", "cat(x)")
	require.Equal(t, "1", strings.ReplaceAll(after.stdout, "\r\n", "\n"), "pre-interrupt state must persist")
}
