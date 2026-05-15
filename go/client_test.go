package main

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
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

// ---- pkgPattern ----

func TestPkgPattern(t *testing.T) {
	hits := []string{
		"Pkg.add(\"Example\")",
		"using Pkg; Pkg.update()",
		"Pkg.resolve()",
	}
	misses := []string{
		"println(\"hello\")",
		"x = 1 + 2",
		"# no package ops here",
	}
	for _, s := range hits {
		require.Truef(t, pkgPattern.MatchString(s), "pkgPattern should match %q", s)
	}
	for _, s := range misses {
		require.Falsef(t, pkgPattern.MatchString(s), "pkgPattern should not match %q", s)
	}
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
	projectSession := newJuliaSession("@temp", "sentinel", "", nil)
	namedSession := newJuliaSession("@temp", "sentinel", "julia +1.12", nil)
	deadSession := newJuliaSession("@shareAnyname", "sentinel", "", nil)
	deadSession.dead.Store(true)
	state.manager.sessions["@temp"] = projectSession
	state.manager.sessions["~temp"] = namedSession
	state.manager.sessions["@shareAnyname"] = deadSession

	resp := handleRequest(state, protocolRequest{Action: "sessions"})
	require.Empty(t, resp.Error)
	require.Equal(t, `Active Julia sessions:
  project @shareAnyname status=dead
  project @temp
  session temp project=@temp julia_cmd=julia +1.12`, resp.Output)
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
	socketDir, err := os.MkdirTemp("/tmp", "julia-client-test-")
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

// TestScriptFile exercises the full main() routing: julia-client script.jl
// The test binary re-invokes itself as the CLI via the TestMain/TEST_CLI mechanism.
func TestScriptFile(t *testing.T) {
	socketPath, stop, _ := startTestDaemon(t)
	defer stop()

	cmd := exec.Command(os.Args[0], "--socket", socketPath, "testdata/compute.jl")
	cmd.Env = append(os.Environ(), "TEST_CLI=1")
	out, err := cmd.Output()
	stderr := ""
	if err != nil {
		if e, ok := err.(*exec.ExitError); ok {
			stderr = string(e.Stderr)
		}
	}
	require.NoErrorf(t, err, "script stderr:\n%s", stderr)
	require.Equal(t, "42\n", string(out))
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
	require.Contains(t, trace.Output, "eval_user_input")

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
	resp = send("println(TestRevPkg.greet())")
	require.Equal(t, "goodbye\n", resp.Output)
}
