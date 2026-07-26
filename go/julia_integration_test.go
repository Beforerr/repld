package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Beforerr/repld/go/julia"
	"github.com/stretchr/testify/require"
)

// streamRequest sends a request expecting NDJSON streaming frames. It returns
// the chunks (each with arrival timestamp) and the terminal frame.
func streamRequest(t *testing.T, socketPath string, req protocolRequest) (chunks []streamChunk, final streamFrame) {
	t.Helper()
	if req.Lang == "" && req.Exe == "" {
		req.Lang = "julia"
	}
	conn, err := net.Dial("unix", socketPath)
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, json.NewEncoder(conn).Encode(req))
	dec := json.NewDecoder(conn)
	for {
		var f streamFrame
		if err := dec.Decode(&f); err != nil {
			require.Fail(t, "decode failed before done frame", err.Error())
		}
		if f.Done {
			final = f
			return
		}
		chunks = append(chunks, streamChunk{data: f.Chunk, stderr: f.Stderr, at: time.Now()})
	}
}

type streamChunk struct {
	data   string
	stderr string
	at     time.Time
}

func TestJuliaWarmSession(t *testing.T) {
	socketPath := sharedDaemon(t)
	cwd := sharedJuliaCwd(t)

	t.Run("eval state and display", func(t *testing.T) {
		res := repldOK(t, socketPath, cwd, "julia", "-e", `println("hello world")`)
		require.Equal(t, "hello world\n", res.stdout)
		require.Empty(t, res.stderr)

		res = repldOK(t, socketPath, cwd, "julia", "-e", "x = 42")
		require.Empty(t, res.stdout)
		res = repldOK(t, socketPath, cwd, "julia", "-e", "println(x)")
		require.Equal(t, "42\n", res.stdout)

		res = repldOK(t, socketPath, cwd, "julia", "-e", `print("no-nl")`)
		require.Equal(t, "no-nl", res.stdout)
		res = repldOK(t, socketPath, cwd, "julia", "-e", `println("with-nl")`)
		require.Equal(t, "with-nl\n", res.stdout)

		res = repldOK(t, socketPath, cwd, "julia", "-E", "1 + 1")
		require.Equal(t, "2\n", res.stdout)
	})

	t.Run("script file", func(t *testing.T) {
		// include() resolves against the session cwd; copy the fixture into the
		// shared cwd so a relative include works without depending on the repo cwd.
		srcScript, err := os.Getwd()
		require.NoError(t, err)
		data, err := os.ReadFile(filepath.Join(srcScript, "testdata", "compute.jl"))
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(filepath.Join(cwd, "testdata"), 0755))
		// Julia reports @__FILE__ as a resolved real path; on macOS the temp dir
		// lives under a /var→/private/var symlink, so resolve to match.
		realCwd, err := filepath.EvalSymlinks(cwd)
		require.NoError(t, err)
		scriptPath := filepath.Join(realCwd, "testdata", "compute.jl")
		require.NoError(t, os.WriteFile(scriptPath, data, 0644))
		scriptDir := filepath.Dir(scriptPath)
		// No file routing layer: include(...) resolves against the session cwd.
		// Julia normalizes @__FILE__ to an absolute path even for a relative include.
		expected := "42\n" + scriptPath + "\n" + scriptDir

		run := func(path string) string {
			t.Helper()
			return repldOK(t, socketPath, cwd, "julia", "-e", fmt.Sprintf("include(%q)", path)).stdout
		}

		require.Equal(t, expected, run(scriptPath), "absolute path")
		// Same warm session; relative include resolves against the session cwd and re-runs.
		require.Equal(t, expected, run("testdata/compute.jl"), "relative path resolved against session cwd")
	})

	t.Run("missing script", func(t *testing.T) {
		res := repldErr(t, socketPath, cwd, "julia", "nonexistent.jl")
		require.Contains(t, res.stderr, "No such file")
	})

	t.Run("trace saved", func(t *testing.T) {
		res := repldErr(t, socketPath, cwd, "julia", "-e", `let f = () -> error("boom"); g = () -> f(); g(); end`)
		require.Empty(t, res.stdout)
		require.Contains(t, res.stderr, "boom")
		require.Contains(t, res.stderr, "Stacktrace:")
		hint := regexp.MustCompile("Trace saved: `repld trace ([k-z]{8})`\\.").FindStringSubmatch(res.stderr)
		require.Len(t, hint, 2)
		require.NotContains(t, res.stderr, "eval_user_input")

		res = repldOK(t, socketPath, cwd, "trace", "--trace", "smart", hint[1])
		require.Contains(t, res.stdout, "Stacktrace:")
		require.Contains(t, res.stdout, "repld-eval")
		require.NotContains(t, res.stdout, "eval_user_input")

		res = repldOK(t, socketPath, cwd, "trace", "--trace", "full", hint[1])
		require.Contains(t, res.stdout, "include_string")

		res = repldErr(t, socketPath, cwd, "--trace", "full", "julia", "-e", `let f = () -> error("boom"); g = () -> f(); g(); end`)
		require.Contains(t, res.stderr, "Stacktrace:")
	})

	t.Run("test failure noise", func(t *testing.T) {
		res := repldErr(t, socketPath, cwd, "julia", "-e", `using Test; @testset "x" begin @test false end`)
		require.Contains(t, res.stdout, "x: Test Failed")
		require.Contains(t, res.stdout, "Test Summary:")
		require.Equal(t, "ERROR: Some tests did not pass: 0 passed, 1 failed, 0 errored, 0 broken.\n", res.stderr)
		require.NotContains(t, res.stderr, "Stacktrace:")
		require.NotContains(t, res.stderr, "Trace saved")
		require.NotContains(t, res.stderr, "TestSetException")

		// `full` keeps the LoadError context that `short` strips.
		res = repldOK(t, socketPath, cwd, "trace", "--trace", "full", "julia")
		require.Contains(t, res.stdout, "in expression starting at")

		res = repldErr(t, socketPath, cwd, "julia", "-e", `using Test; @test false`)
		require.Contains(t, res.stdout, "Test Failed")
		require.Equal(t, "ERROR: There was an error during testing\n", res.stderr)
		require.NotContains(t, res.stderr, "Stacktrace:")
		require.NotContains(t, res.stderr, "FallbackTestSetException")
	})

	t.Run("streaming chunks", func(t *testing.T) {
		code := `println("a"); flush(stdout); sleep(0.3); println("b"); flush(stdout); sleep(0.3); println("c"); flush(stdout)`
		chunks, final := streamRequest(t, socketPath, protocolRequest{Action: "eval", Code: code, Cwd: cwd})
		require.True(t, final.Done)
		require.Empty(t, final.Error)

		var combined string
		for _, c := range chunks {
			combined += c.data
		}
		require.Contains(t, combined, "a")
		require.Contains(t, combined, "b")
		require.Contains(t, combined, "c")

		var aTime, bTime time.Time
		for _, c := range chunks {
			if aTime.IsZero() && strings.Contains(c.data, "a") {
				aTime = c.at
			}
			if bTime.IsZero() && strings.Contains(c.data, "b") {
				bTime = c.at
			}
		}
		require.Falsef(t, aTime.IsZero(), "no chunk contained 'a'; chunks=%v", chunks)
		require.Falsef(t, bTime.IsZero(), "no chunk contained 'b'; chunks=%v", chunks)
		require.Greaterf(t, bTime.Sub(aTime), 150*time.Millisecond,
			"'b' should arrive measurably after 'a' (actual gap %v)", bTime.Sub(aTime))
	})

	t.Run("streaming stderr split", func(t *testing.T) {
		chunks, final := streamRequest(t, socketPath, protocolRequest{
			Action: "eval",
			Code:   `println(stdout, "OUT_LINE"); flush(stdout); println(stderr, "ERR_LINE"); flush(stderr)`,
			Cwd:    cwd,
		})
		require.True(t, final.Done)
		require.Empty(t, final.Error)

		var stdoutBuf, stderrBuf strings.Builder
		for _, c := range chunks {
			stdoutBuf.WriteString(c.data)
			stderrBuf.WriteString(c.stderr)
		}
		require.Contains(t, stdoutBuf.String(), "OUT_LINE")
		require.NotContains(t, stdoutBuf.String(), "ERR_LINE", "stderr output must not leak into stdout chunks")
		require.Contains(t, stderrBuf.String(), "ERR_LINE")
		require.NotContains(t, stderrBuf.String(), "OUT_LINE", "stdout output must not leak into stderr chunks")
	})

	t.Run("streaming error", func(t *testing.T) {
		_, final := streamRequest(t, socketPath, protocolRequest{Action: "eval", Code: `error("boom")`, Cwd: cwd})
		require.True(t, final.Done)
		require.Contains(t, final.Error, "boom")
	})

	t.Run("fresh clears state", func(t *testing.T) {
		repldOK(t, socketPath, cwd, "julia", "-e", "fresh_marker = 42")
		res := repldOK(t, socketPath, cwd, "--fresh", "julia", "-e", "println(isdefined(Main, :fresh_marker))")
		require.Equal(t, "false\n", res.stdout)
	})
}

func TestInterruptBusyAndUnblocks(t *testing.T) {
	socketPath := sharedDaemon(t)
	cwd := sharedJuliaCwd(t)

	// Prime the session and set state that must survive the interrupt.
	repldOK(t, socketPath, cwd, "julia", "-e", "marker = 1234")

	cmd := exec.Command(os.Args[0], "--socket", socketPath, "julia", "-e", "sleep(60)")
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
	go func() {
		done <- cmd.Wait()
	}()

	deadline := time.Now().Add(5 * time.Second)
	var sessOut string
	for time.Now().Before(deadline) {
		sessOut = repldOK(t, socketPath, cwd, "sessions").stdout
		if strings.Contains(sessOut, "status: busy") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.Contains(t, sessOut, "status: busy", "sessions listing should show status: busy while a call is in flight")

	irq := repldOK(t, socketPath, cwd, "interrupt", "julia")
	require.Contains(t, irq.stdout, "interrupted", "sleep must survive the interrupt, got: %q", irq.stdout)
	require.NotContains(t, irq.stdout, "killed")

	select {
	case err := <-done:
		require.Error(t, err)
		require.Contains(t, sleepStderr.String(), "InterruptException")
	case <-time.After(5 * time.Second):
		require.Fail(t, "interrupted call did not return")
	}

	after := repldOK(t, socketPath, cwd, "julia", "-e", "print(marker)")
	require.Equal(t, "1234", after.stdout, "pre-interrupt state must persist")
}

func TestInterruptIdleSession(t *testing.T) {
	socketPath := sharedDaemon(t)
	cwd := sharedJuliaCwd(t)

	repldOK(t, socketPath, cwd, "julia", "-e", "marker = 7")

	irq := repldOK(t, socketPath, cwd, "interrupt", "julia")
	require.Contains(t, irq.stdout, "nothing to interrupt")

	after := repldOK(t, socketPath, cwd, "julia", "-e", "print(marker)")
	require.Equal(t, "7", after.stdout, "idle interrupt must not disturb the session")
}

// covers `timeout 30 repld ...` scenario
func TestClientDisconnectInterruptsEval(t *testing.T) {
	socketPath := sharedDaemon(t)
	cwd := sharedJuliaCwd(t)

	// Set state that must survive the disconnect-interrupt.
	repldOK(t, socketPath, cwd, "julia", "-e", "marker = 5678")

	// Start a long eval on its own connection.
	conn, err := net.Dial("unix", socketPath)
	require.NoError(t, err)
	require.NoError(t, json.NewEncoder(conn).Encode(protocolRequest{
		Action: "eval", Lang: "julia", Code: "sleep(60)", Cwd: cwd,
	}))

	// Wait until the session reports busy.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(sendRequest(t, socketPath, protocolRequest{Action: "sessions"}).Output, "status: busy") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Drop the connection abruptly, as `timeout` killing the client would.
	require.NoError(t, conn.Close())

	// The session must stop being busy promptly. Without disconnect handling,
	// the orphaned sleep(60) would keep the session busy for ~60s.
	require.Eventually(t, func() bool {
		return !strings.Contains(sendRequest(t, socketPath, protocolRequest{Action: "sessions"}).Output, "status: busy")
	}, 10*time.Second, 100*time.Millisecond, "client disconnect should have interrupted the orphaned eval")

	// Session must be usable again AND have survived with state intact
	doneCh := make(chan cliResult, 1)
	go func() {
		doneCh <- repldCLI(t, socketPath, cwd, "julia", "-e", "print(marker)")
	}()
	select {
	case r := <-doneCh:
		require.NoErrorf(t, r.err, "stderr:\n%s", r.stderr)
		require.Equal(t, "5678", r.stdout, "session must survive disconnect-interrupt with state intact")
	case <-time.After(40 * time.Second):
		require.Fail(t, "follow-up eval blocked — session was not freed after client disconnect")
	}
}

func TestRevisePicksUpPackageChanges(t *testing.T) {
	socketPath := sharedDaemon(t)

	pkgDir := sessionCwd(t)
	srcDir := filepath.Join(pkgDir, "src")
	require.NoError(t, os.Mkdir(srcDir, 0755))
	projectToml, err := os.ReadFile(filepath.Join("testdata", "TestRevPkg", "Project.toml"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(pkgDir, "Project.toml"), projectToml, 0644))

	srcFile := filepath.Join(srcDir, "TestRevPkg.jl")
	writePackage := func(greeting string) {
		t.Helper()
		err := os.WriteFile(srcFile, []byte("module TestRevPkg\ngreet() = "+greeting+"\nend\n"), 0644)
		require.NoError(t, err)
	}
	writePackage(`"hello"`)

	send := func(code string) cliResult {
		t.Helper()
		return repldOK(t, socketPath, pkgDir, "julia", "-e", code)
	}

	send("using TestRevPkg")
	resp := send("println(TestRevPkg.greet())")
	require.Equal(t, "hello\n", resp.stdout)

	writePackage(`"goodbye"`)
	// Revise sees the rewrite asynchronously (FSEvents/mtime latency), so the
	// reload is eventually-consistent; each eval runs Revise.revise() first, so
	// retry until the new definition lands.
	require.Eventually(t, func() bool {
		return send("println(TestRevPkg.greet())").stdout == "goodbye\n"
	}, 15*time.Second, 250*time.Millisecond, "Revise did not pick up the package change")
}

// TestJuliaWorldAgeDisplay: a `using` that precompiles bumps world age past run()'s
// frame; showing a result whose method was just defined (an @enum's namemap) must not
// throw "method too new". Guards the Base.invokelatest wrap in runtime.jl.
func TestJuliaWorldAgeDisplay(t *testing.T) {
	socketPath := sharedDaemon(t)

	pkgDir := sessionCwd(t)
	require.NoError(t, os.Mkdir(filepath.Join(pkgDir, "src"), 0755))
	uuid := fmt.Sprintf("a1b2c3d4-0000-0000-0000-%012d", time.Now().UnixNano()%1e12)
	require.NoError(t, os.WriteFile(filepath.Join(pkgDir, "Project.toml"),
		[]byte("name = \"WAEnum\"\nuuid = \""+uuid+"\"\nversion = \"0.0.1\"\n"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(pkgDir, "src", "WAEnum.jl"),
		[]byte("module WAEnum\n@enum Shade Dark=0 Light=1\nmkval() = Dark\nend\n"), 0644))

	// Fresh pkg content forces a cache-miss precompile during this eval (the world bump).
	res := repldOK(t, socketPath, pkgDir, "julia", "--project="+pkgDir, "-E", "using WAEnum; WAEnum.mkval()")
	require.Equal(t, "Dark::Shade = 0\n", res.stdout)
	require.Equal(t, "Dark::Shade = 0\n", repldOK(t, socketPath, pkgDir, "julia", "-E", "WAEnum.mkval()").stdout)
}

// TestKillRunsAtexitHooks: a graceful shutdown must let Julia run its atexit
// hooks (flush buffers, finalizers) rather than SIGKILL the process. The hook
// writes a marker file; its presence proves the process exited cleanly.
func TestKillRunsAtexitHooks(t *testing.T) {
	cwd, err := os.Getwd()
	require.NoError(t, err)
	sess := newSession("julia", newSentinel(), nil, nil)
	require.NoError(t, sess.start("", cwd))

	marker := filepath.Join(t.TempDir(), "atexit.marker")
	require.NoError(t, sess.execute(context.Background(), fmt.Sprintf(`atexit(() -> write(%q, "bye"))`, marker), false, nil))

	sess.kill()
	require.FileExists(t, marker, "graceful shutdown should run atexit hooks, not SIGKILL")
}

func TestJuliaFileEvalArgsAndState(t *testing.T) {
	if _, err := exec.LookPath(julia.Adapter{}.DefaultExe()); err != nil {
		t.Skipf("%s not installed", julia.Adapter{}.DefaultExe())
	}
	socketPath := sharedDaemon(t)

	cwd := sharedJuliaCwd(t)
	script := filepath.Join(t.TempDir(), "script.jl")
	write := func(body string) { require.NoError(t, os.WriteFile(script, []byte(body), 0644)) }
	write("println(join(ARGS, \",\"))\nfile_marker = 11\n")

	res := repldOK(t, socketPath, cwd, "julia", script, "a", "b")
	require.Equal(t, "a,b\n", res.stdout)
	check := repldOK(t, socketPath, cwd, "julia", "-e", "println(file_marker)")
	require.Equal(t, "11\n", check.stdout)
	check = repldOK(t, socketPath, cwd, "julia", "-e", "println(length(ARGS))")
	require.Equal(t, "0\n", check.stdout)

	write("println(\"edited\")\n")
	res = repldOK(t, socketPath, cwd, "julia", script)
	require.Equal(t, "edited\n", res.stdout)
}

// A request queued behind another eval must not interrupt running eval when client disconnects.
func TestQueuedDisconnectDoesNotInterruptRunningEval(t *testing.T) {
	socketPath := sharedDaemon(t)
	cwd := sharedJuliaCwd(t)
	repldOK(t, socketPath, cwd, "julia", "-e", "1")

	// conn1: long enough to still be busy when conn2 queues and disconnects.
	conn1, err := net.Dial("unix", socketPath)
	require.NoError(t, err)
	defer conn1.Close()
	require.NoError(t, json.NewEncoder(conn1).Encode(protocolRequest{
		Action: "eval", Lang: "julia", Cwd: cwd,
		Code: "sleep(1.5); 4321", PrintResult: true,
	}))

	require.Eventually(t, func() bool {
		return strings.Contains(sendRequest(t, socketPath, protocolRequest{Action: "sessions"}).Output, "status: busy")
	}, 5*time.Second, 50*time.Millisecond, "conn1 eval should be running")

	// conn2: same session, queues behind conn1, then disconnects abruptly.
	conn2, err := net.Dial("unix", socketPath)
	require.NoError(t, err)
	require.NoError(t, json.NewEncoder(conn2).Encode(protocolRequest{
		Action: "eval", Lang: "julia", Cwd: cwd, Code: "1+1",
	}))
	time.Sleep(300 * time.Millisecond)
	require.NoError(t, conn2.Close())

	// conn1 must finish cleanly — not interrupted by conn2's disconnect.
	dec := json.NewDecoder(conn1)
	var out strings.Builder
	for {
		var f streamFrame
		require.NoError(t, dec.Decode(&f))
		if f.Done {
			require.Empty(t, f.Error, "running eval was interrupted by a queued client's disconnect")
			break
		}
		out.WriteString(f.Chunk)
	}
	require.Contains(t, out.String(), "4321")
}
