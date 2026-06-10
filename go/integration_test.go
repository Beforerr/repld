package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Beforerr/repld/go/julia"
	"github.com/stretchr/testify/require"
)

type nopWriteCloser struct {
	bytes.Buffer
}

func (w *nopWriteCloser) Close() error { return nil }

// TestMain allows the test binary to act as the CLI when TEST_CLI=1,
// enabling subprocess-based end-to-end tests of main().
func TestMain(m *testing.M) {
	if os.Getenv("TEST_CLI") == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

func newTestState() *daemonState {
	s := &daemonState{
		manager: newSessionManager(),
		stopCh:  make(chan struct{}),
	}
	s.lastRequest.Store(time.Now().UnixNano())
	return s
}

// ---- helpers ----

// startTestDaemon launches serveDaemon in a goroutine and returns a stop func and the socket path.
// The returned WaitGroup is done when the daemon exits.
func startTestDaemon(t *testing.T) (socketPath string, stop func(), wg *sync.WaitGroup) {
	t.Helper()
	// Keep the AF_UNIX path short (macOS caps it near 104 chars); /tmp is short
	// on Unix, but doesn't exist on Windows, so fall back to the OS temp dir.
	base := "/tmp"
	if runtime.GOOS == "windows" {
		base = os.TempDir()
	}
	socketDir, err := os.MkdirTemp(base, "repld-test-")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(socketDir) })
	socketPath = filepath.Join(socketDir, "test.sock")
	errCh := make(chan error, 1)
	wg = &sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		errCh <- serveDaemon(socketPath, time.Hour)
	}()
	waitForSocket(t, socketPath, errCh)
	stop = func() {
		conn, _ := net.Dial("unix", socketPath)
		if conn != nil {
			json.NewEncoder(conn).Encode(protocolRequest{Action: "stop"})
			conn.Close()
		}
		wg.Wait()
	}
	return
}

func waitForSocket(t *testing.T, socketPath string, errCh <-chan error) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-errCh:
			require.NoError(t, err)
		default:
		}
		if _, err := os.Stat(socketPath); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.Fail(t, "daemon socket did not appear in time")
}

func sendRequest(t *testing.T, socketPath string, req protocolRequest) response {
	t.Helper()
	if req.Lang == "" && req.Exe == "" { // these are Julia integration tests
		req.Lang = "julia"
	}
	conn, err := net.Dial("unix", socketPath)
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, json.NewEncoder(conn).Encode(req))
	if req.Action == "eval" {
		// Eval always streams: collect frames into a single response.
		dec := json.NewDecoder(conn)
		var buf strings.Builder
		for {
			var f streamFrame
			require.NoError(t, dec.Decode(&f))
			if f.Done {
				return response{Output: buf.String(), Error: f.Error}
			}
			buf.WriteString(f.Chunk)
		}
	}
	var resp response
	require.NoError(t, json.NewDecoder(conn).Decode(&resp))
	return resp
}

type cliResult struct {
	stdout string
	stderr string
	err    error
}

func repldCLI(t *testing.T, socketPath, cwd string, args ...string) cliResult {
	t.Helper()
	cmd := exec.Command(os.Args[0], append([]string{"--socket", socketPath}, args...)...)
	cmd.Env = append(os.Environ(), "TEST_CLI=1")
	cmd.Dir = cwd
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return cliResult{stdout: stdout.String(), stderr: stderr.String(), err: err}
}

func repldOK(t *testing.T, socketPath, cwd string, args ...string) cliResult {
	t.Helper()
	res := repldCLI(t, socketPath, cwd, args...)
	require.NoErrorf(t, res.err, "stdout:\n%s\nstderr:\n%s", res.stdout, res.stderr)
	return res
}

func repldErr(t *testing.T, socketPath, cwd string, args ...string) cliResult {
	t.Helper()
	res := repldCLI(t, socketPath, cwd, args...)
	require.Errorf(t, res.err, "stdout:\n%s\nstderr:\n%s", res.stdout, res.stderr)
	return res
}

func TestDaemonPingOverSocket(t *testing.T) {
	socketPath, stop, _ := startTestDaemon(t)
	defer stop()

	resp := sendRequest(t, socketPath, protocolRequest{Action: "ping"})
	require.Equal(t, "pong", resp.Output)
}

func tempSocketPath(t *testing.T) string {
	t.Helper()
	base := "/tmp"
	if runtime.GOOS == "windows" {
		base = os.TempDir()
	}
	dir, err := os.MkdirTemp(base, "repld-test-")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "test.sock")
}

// A crashed daemon leaves a socket file with no listener. serveDaemon must treat
// it as stale: remove and bind, not give up.
func TestServeDaemonReplacesStaleSocket(t *testing.T) {
	socketPath := tempSocketPath(t)
	// Bind then close WITHOUT removing: the file lingers but nothing listens.
	stale, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	addr := stale.Addr().(*net.UnixAddr)
	require.NoError(t, stale.Close())
	if _, err := os.Stat(addr.Name); err != nil {
		require.NoError(t, os.WriteFile(socketPath, []byte("stale"), 0600))
	}
	require.False(t, pingDaemon(socketPath), "stale socket must not answer ping")

	errCh := make(chan error, 1)
	wg := &sync.WaitGroup{}
	wg.Add(1)
	go func() { defer wg.Done(); errCh <- serveDaemon(socketPath, time.Hour) }()
	// Poll on ping, not file existence: a pre-existing stale file passes os.Stat
	// before the daemon swaps in its real socket.
	require.Eventually(t, func() bool { return pingDaemon(socketPath) }, 15*time.Second, 20*time.Millisecond)

	resp := sendRequest(t, socketPath, protocolRequest{Action: "ping"})
	require.Equal(t, "pong", resp.Output)

	conn, _ := net.Dial("unix", socketPath)
	json.NewEncoder(conn).Encode(protocolRequest{Action: "stop"})
	conn.Close()
	wg.Wait()
	require.NoError(t, <-errCh)
}

func TestServeDaemonRefusesLiveDaemon(t *testing.T) {
	socketPath, stop, _ := startTestDaemon(t)

	err := serveDaemon(socketPath, time.Hour)
	require.Error(t, err)
	require.Contains(t, err.Error(), "already running")

	// First daemon's socket survived
	resp := sendRequest(t, socketPath, protocolRequest{Action: "ping"})
	require.Equal(t, "pong", resp.Output)

	stop()
}

// ---- Julia integration ----
func TestJuliaWarmSession(t *testing.T) {
	socketPath, stop, _ := startTestDaemon(t)
	defer stop()

	cwd, err := os.Getwd()
	require.NoError(t, err)

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
		scriptPath := filepath.Join(cwd, "testdata", "compute.jl")
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

	t.Run("trace saved", func(t *testing.T) {
		res := repldErr(t, socketPath, cwd, "julia", "-e", `let f = () -> error("boom"); g = () -> f(); g(); end`)
		require.Empty(t, res.stdout)
		require.Contains(t, res.stderr, "boom")
		require.Contains(t, res.stderr, "Stacktrace:")
		require.Contains(t, res.stderr, "run `trace")
		require.NotContains(t, res.stderr, "eval_user_input")

		res = repldOK(t, socketPath, cwd, "trace", "--trace", "smart", "julia")
		require.Contains(t, res.stdout, "Stacktrace:")
		require.Contains(t, res.stdout, "repld-eval")
		require.NotContains(t, res.stdout, "eval_user_input")

		res = repldOK(t, socketPath, cwd, "trace", "--trace", "full", "julia")
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

// TestPerSessionThreads: a forwarded -t switch sets the thread count per session
// at creation, so distinct sessions get independent counts; a reused session
// keeps the count it was born with (a live process can't be re-threaded).
func TestPerSessionThreads(t *testing.T) {
	socketPath, stop, _ := startTestDaemon(t)
	defer stop()

	cwd, err := os.Getwd()
	require.NoError(t, err)
	nthreads := func(session, threads string) cliResult {
		return repldOK(t, socketPath, cwd, "--session", session, "julia", "-t", threads, "-e", "print(Threads.nthreads())")
	}

	a := nthreads("thr-a", "2")
	require.Equal(t, "2", a.stdout)

	// A second, distinct session gets its own count.
	b := nthreads("thr-b", "3")
	require.Equal(t, "3", b.stdout)

	// Reusing thr-a ignores the new value — the process keeps its launch count.
	again := nthreads("thr-a", "4")
	require.Equal(t, "2", again.stdout)
}

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

// TestInterruptBusyAndUnblocks: sessions reports busy= during a call; interrupt
// unblocks it as a catchable InterruptException; the session survives with
// pre-interrupt state intact.
func TestInterruptBusyAndUnblocks(t *testing.T) {
	socketPath, stop, _ := startTestDaemon(t)
	defer stop()

	cwd, err := os.Getwd()
	require.NoError(t, err)

	// Prime the session and set state that must survive the interrupt.
	repldOK(t, socketPath, cwd, "--session", "irq", "julia", "-e", "marker = 1234")

	cmd := exec.Command(os.Args[0], "--socket", socketPath, "--session", "irq", "julia", "-e", "sleep(60)")
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
		if strings.Contains(sessOut, "busy=") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.Contains(t, sessOut, "busy=", "sessions listing should show busy= while a call is in flight")

	irq := repldOK(t, socketPath, cwd, "interrupt", "--session", "irq")
	require.Contains(t, irq.stdout, "interrupted", "sleep must survive the interrupt, got: %q", irq.stdout)
	require.NotContains(t, irq.stdout, "killed")

	select {
	case err := <-done:
		require.Error(t, err)
		require.Contains(t, sleepStderr.String(), "InterruptException")
	case <-time.After(5 * time.Second):
		require.Fail(t, "interrupted call did not return")
	}

	after := repldOK(t, socketPath, cwd, "--session", "irq", "julia", "-e", "print(marker)")
	require.Equal(t, "1234", after.stdout, "pre-interrupt state must persist")
}

// covers `timeout 30 repld ...` scenario
func TestClientDisconnectInterruptsEval(t *testing.T) {
	socketPath, stop, _ := startTestDaemon(t)
	defer stop()

	cwd, err := os.Getwd()
	require.NoError(t, err)

	// Prime the session and set state that must survive the disconnect-interrupt.
	repldOK(t, socketPath, cwd, "--session", "disc", "julia", "-e", "marker = 5678")

	// Start a long eval on its own connection.
	conn, err := net.Dial("unix", socketPath)
	require.NoError(t, err)
	require.NoError(t, json.NewEncoder(conn).Encode(protocolRequest{
		Action: "eval", Lang: "julia", Session: "disc", Code: "sleep(60)", Cwd: cwd,
	}))

	// Wait until the session reports busy.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(sendRequest(t, socketPath, protocolRequest{Action: "sessions"}).Output, "busy=") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Drop the connection abruptly, as `timeout` killing the client would.
	require.NoError(t, conn.Close())

	// The session must stop being busy promptly. Without disconnect handling,
	// the orphaned sleep(60) would keep the session busy for ~60s.
	require.Eventually(t, func() bool {
		return !strings.Contains(sendRequest(t, socketPath, protocolRequest{Action: "sessions"}).Output, "busy=")
	}, 10*time.Second, 100*time.Millisecond, "client disconnect should have interrupted the orphaned eval")

	// Session must be usable again AND have survived with state intact
	doneCh := make(chan cliResult, 1)
	go func() {
		doneCh <- repldCLI(t, socketPath, cwd, "--session", "disc", "julia", "-e", "print(marker)")
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
	socketPath, stop, _ := startTestDaemon(t)
	defer stop()

	pkgDir := t.TempDir()
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
	socketPath, stop, _ := startTestDaemon(t)
	defer stop()

	pkgDir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(pkgDir, "src"), 0755))
	uuid := fmt.Sprintf("a1b2c3d4-0000-0000-0000-%012d", time.Now().UnixNano()%1e12)
	require.NoError(t, os.WriteFile(filepath.Join(pkgDir, "Project.toml"),
		[]byte("name = \"WAEnum\"\nuuid = \""+uuid+"\"\nversion = \"0.0.1\"\n"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(pkgDir, "src", "WAEnum.jl"),
		[]byte("module WAEnum\n@enum Shade Dark=0 Light=1\nmkval() = Dark\nend\n"), 0644))

	// Fresh pkg content forces a cache-miss precompile during this eval (the world bump).
	res := repldOK(t, socketPath, pkgDir, "julia", "--project="+pkgDir, "-E", "using WAEnum; WAEnum.mkval()")
	require.Equal(t, "Dark::Shade = 0\n", res.stdout)
	require.NotContains(t, res.stderr, "world age")
	require.NotContains(t, res.stderr, "too new")
}

// TestKillRunsAtexitHooks: a graceful shutdown must let Julia run its atexit
// hooks (flush buffers, finalizers) rather than SIGKILL the process. The hook
// writes a marker file; its presence proves the process exited cleanly.
func TestKillRunsAtexitHooks(t *testing.T) {
	cwd, err := os.Getwd()
	require.NoError(t, err)
	sess := newSession(julia.Adapter{}, newSentinel(), nil, nil)
	require.NoError(t, sess.start("", cwd))

	marker := filepath.Join(t.TempDir(), "atexit.marker")
	require.NoError(t, sess.execute(context.Background(), fmt.Sprintf(`atexit(() -> write(%q, "bye"))`, marker), false, nil))

	sess.kill()
	require.FileExists(t, marker, "graceful shutdown should run atexit hooks, not SIGKILL")
}

func TestNoInterpreterDoesNotCreateMissingNamedSession(t *testing.T) {
	socketPath, stop, _ := startTestDaemon(t)
	defer stop()

	resp := sendRequest(t, socketPath, protocolRequest{
		Action: "eval", Session: "missing", Code: "1", Cwd: t.TempDir(), RequireExisting: true,
	})
	require.NotEmpty(t, resp.Error)
	require.Contains(t, resp.Error, "no existing session")
}

func TestCLINoInterpreterDoesNotCreateMissingNamedSession(t *testing.T) {
	socketPath, stop, _ := startTestDaemon(t)
	defer stop()

	res := repldErr(t, socketPath, "", "--session", "missing", "-c", "1")
	require.Contains(t, res.stderr, "no existing session")
}

func TestCLIJuliaMissingScriptBehavesLikeInteractiveInterpreter(t *testing.T) {
	if _, err := exec.LookPath(julia.Adapter{}.DefaultExe()); err != nil {
		t.Skipf("%s not installed", julia.Adapter{}.DefaultExe())
	}
	socketPath, stop, _ := startTestDaemon(t)
	defer stop()

	res := repldOK(t, socketPath, "", "julia", "x=1")
	require.Contains(t, res.stderr, "No such file")
	require.NotContains(t, res.stderr, "repld: persistent REPL daemon")
}

func TestExecuteRawWithoutControlDoesNotPanic(t *testing.T) {
	sess := newSession(julia.Adapter{}, "SENTINEL", nil, nil)
	sess.stdin = &nopWriteCloser{}
	sess.stdout = bufio.NewReader(strings.NewReader("SENTINEL\n"))
	sess.stderr = bufio.NewReader(strings.NewReader("SENTINEL\n"))

	_, err := sess.executeRaw("1", nil, true, 1)
	require.Error(t, err)
	require.Contains(t, err.Error(), "control")
}

// A request queued behind another eval must not interrupt running eval when client disconnects.
func TestQueuedDisconnectDoesNotInterruptRunningEval(t *testing.T) {
	socketPath, stop, _ := startTestDaemon(t)
	defer stop()

	cwd, err := os.Getwd()
	require.NoError(t, err)
	repldOK(t, socketPath, cwd, "--session", "q", "julia", "-e", "1")

	// conn1: long enough to still be busy when conn2 queues and disconnects.
	conn1, err := net.Dial("unix", socketPath)
	require.NoError(t, err)
	defer conn1.Close()
	require.NoError(t, json.NewEncoder(conn1).Encode(protocolRequest{
		Action: "eval", Lang: "julia", Session: "q", Cwd: cwd,
		Code: "sleep(4); 4321", PrintResult: true,
	}))

	require.Eventually(t, func() bool {
		return strings.Contains(sendRequest(t, socketPath, protocolRequest{Action: "sessions"}).Output, "busy=")
	}, 5*time.Second, 50*time.Millisecond, "conn1 eval should be running")

	// conn2: same session, queues behind conn1, then disconnects abruptly.
	conn2, err := net.Dial("unix", socketPath)
	require.NoError(t, err)
	require.NoError(t, json.NewEncoder(conn2).Encode(protocolRequest{
		Action: "eval", Lang: "julia", Session: "q", Cwd: cwd, Code: "1+1",
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
