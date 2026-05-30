package main

import (
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

	"github.com/stretchr/testify/require"
)

// TestMain allows the test binary to act as the CLI when TEST_CLI=1,
// enabling subprocess-based end-to-end tests of main().
func TestMain(m *testing.M) {
	if os.Getenv("TEST_CLI") == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

// ---- handleRequest (no Julia needed) ----

func newTestState() *daemonState {
	s := &daemonState{
		manager: newSessionManager(),
		stopCh:  make(chan struct{}),
	}
	s.lastRequest.Store(time.Now().UnixNano())
	return s
}

func TestHandleRequest_Ping(t *testing.T) {
	state := newTestState()
	resp := handleRequest(state, protocolRequest{Action: "ping"})
	require.Equal(t, "pong", resp.Output)
}

func TestHandleRequest_SessionsEmpty(t *testing.T) {
	state := newTestState()
	resp := handleRequest(state, protocolRequest{Action: "sessions"})
	require.Equal(t, "No active Julia sessions.", resp.Output)
}

func TestHandleRequest_UnknownAction(t *testing.T) {
	state := newTestState()
	resp := handleRequest(state, protocolRequest{Action: "bogus"})
	require.NotEmpty(t, resp.Error)
}

func TestHandleRequest_Stop(t *testing.T) {
	state := newTestState()
	resp := handleRequest(state, protocolRequest{Action: "stop"})
	require.Equal(t, "Daemon stopping.", resp.Output)
	select {
	case <-state.stopCh:
		// closed as expected
	default:
		require.Fail(t, "stopCh not closed after stop action")
	}
}

func TestHandleRequest_SessionsShowsRouteAndProject(t *testing.T) {
	state := newTestState()
	projectSession := newJuliaSession("@temp", "sentinel", nil, nil)
	namedSession := newJuliaSession("@temp", "sentinel", []string{"+1.12", "--startup-file=no"}, nil)
	deadSession := newJuliaSession("@shareAnyname", "sentinel", nil, nil)
	deadSession.dead.Store(true)
	state.manager.sessions["@temp"] = projectSession
	state.manager.sessions["~temp"] = namedSession
	state.manager.sessions["@shareAnyname"] = deadSession

	resp := handleRequest(state, protocolRequest{Action: "sessions"})
	require.Empty(t, resp.Error)
	require.Equal(t, `Active Julia sessions:
  project @shareAnyname status=dead
  project @temp
  session temp project=@temp julia_args=+1.12 --startup-file=no`, resp.Output)
}

func TestParseArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want parsed
	}{
		{"eval", []string{"-e", "1+1"}, parsed{evalMode: "eval", code: "1+1"}},
		{"print long", []string{"--print", "x"}, parsed{evalMode: "print", code: "x"}},
		{"our flags", []string{"--session", "s", "--project", "p", "-t", "4"},
			parsed{session: "s", project: "p", threads: "4"}},
		{"passthrough after ours", []string{"--session", "s", "-L", "init.jl", "-e", "c"},
			parsed{session: "s", evalMode: "eval", code: "c", juliaArgs: []string{"-L", "init.jl"}}},
		{"passthrough before ours", []string{"-L", "init.jl", "-e", "c"},
			parsed{evalMode: "eval", code: "c", juliaArgs: []string{"-L", "init.jl"}}},
		{"eq form forwarded whole", []string{"--startup-file=no"},
			parsed{juliaArgs: []string{"--startup-file=no"}}},
		{"channel forwarded first", []string{"+1.11", "-e", "c"},
			parsed{evalMode: "eval", code: "c", juliaArgs: []string{"+1.11"}}},
		{"file positional", []string{"script.jl"}, parsed{files: []string{"script.jl"}}},
		{"subcommand", []string{"sessions"}, parsed{sub: "sessions"}},
		{"flags before subcommand", []string{"--socket", "x", "sessions"},
			parsed{socket: "x", sub: "sessions"}},
		{"subcommand after passthrough is forwarded", []string{"-L", "a.jl", "sessions"},
			parsed{juliaArgs: []string{"-L", "a.jl", "sessions"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseArgs(tc.args)
			// socket/project default unless the case overrides them.
			if tc.want.socket == "" {
				tc.want.socket = defaultSocket
			}
			if tc.want.project == "" {
				tc.want.project = "@."
			}
			require.Equal(t, tc.want.socket, got.socket)
			require.Equal(t, tc.want.project, got.project)
			require.Equal(t, tc.want.session, got.session)
			require.Equal(t, tc.want.threads, got.threads)
			require.Equal(t, tc.want.evalMode, got.evalMode)
			require.Equal(t, tc.want.code, got.code)
			require.Equal(t, tc.want.juliaArgs, got.juliaArgs)
			require.Equal(t, tc.want.files, got.files)
			require.Equal(t, tc.want.sub, got.sub)
		})
	}
}

func TestNormalizeProjectArgPreservesJuliaSelectors(t *testing.T) {
	require.Equal(t, "", normalizeProjectArg(""))
	require.Equal(t, "@.", normalizeProjectArg("@."))
	require.Equal(t, "@temp", normalizeProjectArg("@temp"))
	require.Equal(t, "@shareAnyname", normalizeProjectArg("@shareAnyname"))

	abs, err := filepath.Abs("relative-project")
	require.NoError(t, err)
	require.Equal(t, abs, normalizeProjectArg("relative-project"))
}

func TestSessionManagerKeyPreservesJuliaProjectSelectors(t *testing.T) {
	manager := newSessionManager()
	defer manager.shutdown()

	cwd := t.TempDir()
	require.Equal(t, cwd, manager.key("", "@.", cwd))
	require.Equal(t, "@temp", manager.key("", "@temp", cwd))
	require.Equal(t, "@shareAnyname", manager.key("", "@shareAnyname", cwd))
	require.Equal(t, "~scratch", manager.key("scratch", "@temp", cwd))
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
	socketDir, err := os.MkdirTemp(base, "julia-client-test-")
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

// ---- daemon socket integration (no Julia) ----

func TestDaemonPingOverSocket(t *testing.T) {
	socketPath, stop, _ := startTestDaemon(t)
	defer stop()

	resp := sendRequest(t, socketPath, protocolRequest{Action: "ping"})
	require.Equal(t, "pong", resp.Output)
}

// ---- Julia integration ----

func TestEvalBasic(t *testing.T) {
	socketPath, stop, _ := startTestDaemon(t)
	defer stop()

	cwd, err := os.Getwd()
	require.NoError(t, err)
	send := func(req protocolRequest) response {
		if req.Cwd == "" {
			req.Cwd = cwd
		}
		return sendRequest(t, socketPath, req)
	}

	// Eval basic expression
	resp := send(protocolRequest{Action: "eval", Code: `println("hello world")`})
	require.Empty(t, resp.Error)
	require.Equal(t, "hello world\n", resp.Output)

	// State persists across calls
	resp = send(protocolRequest{Action: "eval", Code: "x = 42"})
	require.Empty(t, resp.Error)
	resp2 := send(protocolRequest{Action: "eval", Code: "println(x)"})
	require.Empty(t, resp2.Error)
	require.Equal(t, "42\n", resp2.Output)

	// Fresh eval clears state before running code.
	resp3 := send(protocolRequest{Action: "eval", Code: "println(isdefined(Main, :x))", Fresh: true})
	require.Empty(t, resp3.Error)
	require.Equal(t, "false\n", resp3.Output)

	// println adds trailing newline; print does not
	resp4 := send(protocolRequest{Action: "eval", Code: `print("no-nl")`})
	require.Empty(t, resp4.Error)
	require.Equal(t, "no-nl", resp4.Output)
	resp5 := send(protocolRequest{Action: "eval", Code: `println("with-nl")`})
	require.Empty(t, resp5.Error)
	require.Equal(t, "with-nl\n", resp5.Output)
}

// TestPerSessionThreads: a forwarded -t switch sets the thread count per session
// at creation, so distinct sessions get independent counts; a reused session
// keeps the count it was born with (a live process can't be re-threaded).
func TestPerSessionThreads(t *testing.T) {
	socketPath, stop, _ := startTestDaemon(t)
	defer stop()

	cwd, err := os.Getwd()
	require.NoError(t, err)
	nthreads := func(session, threads string) response {
		return sendRequest(t, socketPath, protocolRequest{
			Action: "eval", Session: session, Cwd: cwd,
			JuliaArgs: []string{"-t", threads},
			Code:      "print(Threads.nthreads())",
		})
	}

	a := nthreads("thr-a", "2")
	require.Empty(t, a.Error)
	require.Equal(t, "2", a.Output)

	// A second, distinct session gets its own count.
	b := nthreads("thr-b", "3")
	require.Empty(t, b.Error)
	require.Equal(t, "3", b.Output)

	// Reusing thr-a ignores the new value — the process keeps its launch count.
	again := nthreads("thr-a", "4")
	require.Empty(t, again.Error)
	require.Equal(t, "2", again.Output)
}

// TestScriptFile exercises the full main() routing: julia-client script.jl
// The test binary re-invokes itself as the CLI via the TestMain/TEST_CLI mechanism.
func TestScriptFile(t *testing.T) {
	socketPath, stop, _ := startTestDaemon(t)
	defer stop()

	cwd, err := os.Getwd()
	require.NoError(t, err)
	scriptPath := filepath.Join(cwd, "testdata", "compute.jl")
	scriptDir := filepath.Dir(scriptPath)
	expected := "42\n" + scriptPath + "\n" + scriptDir

	run := func(arg string) string {
		t.Helper()
		cmd := exec.Command(os.Args[0], "--socket", socketPath, arg)
		cmd.Env = append(os.Environ(), "TEST_CLI=1")
		cmd.Dir = cwd
		out, err := cmd.Output()
		stderr := ""
		if e, ok := err.(*exec.ExitError); ok {
			stderr = string(e.Stderr)
		}
		require.NoErrorf(t, err, "stderr:\n%s", stderr)
		return string(out)
	}

	require.Equal(t, expected, run(scriptPath), "absolute path")
	require.Equal(t, expected, run("testdata/compute.jl"), "relative path resolved against client cwd")
}

func TestPrintResult(t *testing.T) {
	socketPath, stop, _ := startTestDaemon(t)
	defer stop()

	cwd, err := os.Getwd()
	require.NoError(t, err)
	resp := sendRequest(t, socketPath, protocolRequest{
		Action:      "eval",
		Code:        "1 + 1",
		Cwd:         cwd,
		PrintResult: true,
	})
	require.Empty(t, resp.Error)
	require.Equal(t, "2\n", resp.Output)
}

func TestEvalErrorTraceSaved(t *testing.T) {
	socketPath, stop, _ := startTestDaemon(t)
	defer stop()

	cwd, err := os.Getwd()
	require.NoError(t, err)

	resp := sendRequest(t, socketPath, protocolRequest{
		Action: "eval",
		Code:   `let f = () -> error("boom"); g = () -> f(); g(); end`,
		Cwd:    cwd,
	})
	require.Empty(t, resp.Output)
	require.Contains(t, resp.Error, "boom")
	require.Contains(t, resp.Error, "Stacktrace:")
	require.Contains(t, resp.Error, "julia-client trace")
	require.NotContains(t, resp.Error, "eval_user_input")

	trace := sendRequest(t, socketPath, protocolRequest{
		Action:     "trace",
		Cwd:        cwd,
		TraceLevel: "smart",
	})
	require.Empty(t, trace.Error)
	require.Contains(t, trace.Output, "Stacktrace:")
	require.Contains(t, trace.Output, "julia-client-eval")
	require.NotContains(t, trace.Output, "eval_user_input")

	trace = sendRequest(t, socketPath, protocolRequest{
		Action:     "trace",
		Cwd:        cwd,
		TraceLevel: "full",
	})
	require.Empty(t, trace.Error)
	require.Contains(t, trace.Output, "include_string")

	resp = sendRequest(t, socketPath, protocolRequest{
		Action:     "eval",
		Code:       `let f = () -> error("boom"); g = () -> f(); g(); end`,
		Cwd:        cwd,
		TraceLevel: "full",
	})
	require.Contains(t, resp.Error, "Stacktrace:")
}

func TestEvalTestFailureKeepsNativeNoiseLevel(t *testing.T) {
	socketPath, stop, _ := startTestDaemon(t)
	defer stop()

	cwd, err := os.Getwd()
	require.NoError(t, err)

	resp := sendRequest(t, socketPath, protocolRequest{
		Action: "eval",
		Code:   `using Test; @testset "x" begin @test false end`,
		Cwd:    cwd,
	})
	require.Contains(t, resp.Output, "x: Test Failed")
	require.Contains(t, resp.Output, "Test Summary:")
	require.Equal(t, "ERROR: Some tests did not pass: 0 passed, 1 failed, 0 errored, 0 broken.", resp.Error)
	require.NotContains(t, resp.Error, "Stacktrace:")
	require.NotContains(t, resp.Error, "Trace saved")
	require.NotContains(t, resp.Error, "TestSetException")

	trace := sendRequest(t, socketPath, protocolRequest{
		Action:     "trace",
		Cwd:        cwd,
		TraceLevel: "full",
	})
	require.Empty(t, trace.Error)
	require.Contains(t, trace.Output, "TestSetException")

	resp = sendRequest(t, socketPath, protocolRequest{
		Action: "eval",
		Code:   `using Test; @test false`,
		Cwd:    cwd,
	})
	require.Contains(t, resp.Output, "Test Failed")
	require.Equal(t, "ERROR: There was an error during testing", resp.Error)
	require.NotContains(t, resp.Error, "Stacktrace:")
	require.NotContains(t, resp.Error, "FallbackTestSetException")
}

// streamRequest sends a request expecting NDJSON streaming frames. It returns
// the chunks (each with arrival timestamp) and the terminal frame.
func streamRequest(t *testing.T, socketPath string, req protocolRequest) (chunks []streamChunk, final streamFrame) {
	t.Helper()
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

func TestStreamingEvalDeliversChunksIncrementally(t *testing.T) {
	socketPath, stop, _ := startTestDaemon(t)
	defer stop()

	cwd, err := os.Getwd()
	require.NoError(t, err)

	// Prime the session so we don't measure startup cost.
	sendRequest(t, socketPath, protocolRequest{Action: "eval", Code: "1+1", Cwd: cwd})

	code := `println("a"); flush(stdout); sleep(0.3); println("b"); flush(stdout); sleep(0.3); println("c"); flush(stdout)`
	chunks, final := streamRequest(t, socketPath, protocolRequest{
		Action: "eval", Code: code, Cwd: cwd,
	})
	require.True(t, final.Done)
	require.Empty(t, final.Error)

	var combined string
	for _, c := range chunks {
		combined += c.data
	}
	require.Contains(t, combined, "a")
	require.Contains(t, combined, "b")
	require.Contains(t, combined, "c")

	// The chunk(s) carrying "b" must arrive measurably after the chunk(s) with "a".
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
}

func TestStreamingSeparatesStdoutFromStderr(t *testing.T) {
	socketPath, stop, _ := startTestDaemon(t)
	defer stop()
	cwd, err := os.Getwd()
	require.NoError(t, err)
	sendRequest(t, socketPath, protocolRequest{Action: "eval", Code: "1+1", Cwd: cwd})

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
}

func TestStreamingEvalSurfacesError(t *testing.T) {
	socketPath, stop, _ := startTestDaemon(t)
	defer stop()

	cwd, err := os.Getwd()
	require.NoError(t, err)
	sendRequest(t, socketPath, protocolRequest{Action: "eval", Code: "1+1", Cwd: cwd})

	_, final := streamRequest(t, socketPath, protocolRequest{
		Action: "eval", Code: `error("boom")`, Cwd: cwd,
	})
	require.True(t, final.Done)
	require.Contains(t, final.Error, "boom")
}

func TestInterruptUnknownSession(t *testing.T) {
	state := newTestState()
	resp := handleRequest(state, protocolRequest{
		Action:  "interrupt",
		Session: "nope",
		Cwd:     t.TempDir(),
	})
	require.NotEmpty(t, resp.Error)
	require.Contains(t, resp.Error, "no session")
}

// TestInterruptBusyAndUnblocks: sessions reports busy= during a call; interrupt
// unblocks it as a catchable InterruptException; the session survives with
// pre-interrupt state intact.
func TestInterruptBusyAndUnblocks(t *testing.T) {
	socketPath, stop, _ := startTestDaemon(t)
	defer stop()

	cwd, err := os.Getwd()
	require.NoError(t, err)
	send := func(req protocolRequest) response {
		if req.Cwd == "" {
			req.Cwd = cwd
		}
		return sendRequest(t, socketPath, req)
	}

	// Prime the session and set state that must survive the interrupt.
	resp := send(protocolRequest{Action: "eval", Session: "irq", Code: "marker = 1234"})
	require.Empty(t, resp.Error)

	done := make(chan response, 1)
	go func() {
		done <- send(protocolRequest{Action: "eval", Session: "irq", Code: "sleep(60)"})
	}()

	deadline := time.Now().Add(5 * time.Second)
	var sessResp response
	for time.Now().Before(deadline) {
		sessResp = send(protocolRequest{Action: "sessions"})
		if strings.Contains(sessResp.Output, "busy=") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.Contains(t, sessResp.Output, "busy=", "sessions listing should show busy= while a call is in flight")

	irq := send(protocolRequest{Action: "interrupt", Session: "irq"})
	require.Empty(t, irq.Error)
	require.Contains(t, irq.Output, "interrupted", "sleep must survive the interrupt, got: %q", irq.Output)
	require.NotContains(t, irq.Output, "killed")

	select {
	case r := <-done:
		require.Contains(t, r.Error, "InterruptException")
	case <-time.After(5 * time.Second):
		require.Fail(t, "interrupted call did not return")
	}

	after := send(protocolRequest{Action: "eval", Session: "irq", Code: "print(marker)"})
	require.Empty(t, after.Error, "session must survive interrupt, not restart")
	require.Equal(t, "1234", after.Output, "pre-interrupt state must persist")
}

// TestClientDisconnectInterruptsEval covers the `timeout 30 julia-client`
// scenario: when the client is killed mid-eval, the daemon must interrupt the
// in-flight computation rather than let it orphan and hold the session lock.
func TestClientDisconnectInterruptsEval(t *testing.T) {
	socketPath, stop, _ := startTestDaemon(t)
	defer stop()

	cwd, err := os.Getwd()
	require.NoError(t, err)

	// Prime the session and set state that must survive the disconnect-interrupt.
	resp := sendRequest(t, socketPath, protocolRequest{Action: "eval", Session: "disc", Code: "marker = 5678", Cwd: cwd})
	require.Empty(t, resp.Error)

	// Start a long eval on its own connection.
	conn, err := net.Dial("unix", socketPath)
	require.NoError(t, err)
	require.NoError(t, json.NewEncoder(conn).Encode(protocolRequest{
		Action: "eval", Session: "disc", Code: "sleep(60)", Cwd: cwd,
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

	// Session must be usable again AND have survived with state intact (not
	// silently restarted): a follow-up eval reads back the pre-interrupt marker.
	doneCh := make(chan response, 1)
	go func() {
		doneCh <- sendRequest(t, socketPath, protocolRequest{Action: "eval", Session: "disc", Code: "print(marker)", Cwd: cwd})
	}()
	select {
	case r := <-doneCh:
		require.Empty(t, r.Error)
		require.Equal(t, "5678", r.Output, "session must survive disconnect-interrupt with state intact")
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

	send := func(code string) response {
		t.Helper()
		resp := sendRequest(t, socketPath, protocolRequest{
			Action: "eval",
			Code:   code,
			Cwd:    pkgDir,
		})
		require.Empty(t, resp.Error, "eval %q failed", code)
		return resp
	}

	send("using TestRevPkg")
	resp := send("println(TestRevPkg.greet())")
	require.Equal(t, "hello\n", resp.Output)

	writePackage(`"goodbye"`)
	// Revise sees the rewrite asynchronously (FSEvents/mtime latency), so the
	// reload is eventually-consistent; each eval runs Revise.revise() first, so
	// retry until the new definition lands.
	require.Eventually(t, func() bool {
		return send("println(TestRevPkg.greet())").Output == "goodbye\n"
	}, 15*time.Second, 250*time.Millisecond, "Revise did not pick up the package change")
}

// TestKillRunsAtexitHooks: a graceful shutdown must let Julia run its atexit
// hooks (flush buffers, finalizers) rather than SIGKILL the process. The hook
// writes a marker file; its presence proves the process exited cleanly.
func TestKillRunsAtexitHooks(t *testing.T) {
	cwd, err := os.Getwd()
	require.NoError(t, err)
	sess := newJuliaSession(cwd, newSentinel(), nil, nil)
	require.NoError(t, sess.start("", cwd))

	marker := filepath.Join(t.TempDir(), "atexit.marker")
	require.NoError(t, sess.execute(fmt.Sprintf(`atexit(() -> write(%q, "bye"))`, marker), false, nil))

	sess.kill()
	require.FileExists(t, marker, "graceful shutdown should run atexit hooks, not SIGKILL")
}
