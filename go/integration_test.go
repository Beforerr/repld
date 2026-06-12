package main

import (
	"bufio"
	"bytes"
	"encoding/json"
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
	code := m.Run()
	stopSharedDaemon()
	os.Exit(code)
}

var (
	sharedOnce      sync.Once
	sharedSocket    string
	sharedStop      func()
	sharedJuliaDir  string
	sharedJuliaOnce sync.Once
)

func sharedDaemon(t *testing.T) string {
	t.Helper()
	sharedOnce.Do(func() {
		// Not bound to any test's lifetime; torn down in TestMain.
		sharedSocket, sharedStop, _ = startTestDaemon(nil)
	})
	return sharedSocket
}

func stopSharedDaemon() {
	if sharedStop != nil {
		sharedStop()
	}
	if sharedJuliaDir != "" {
		os.RemoveAll(sharedJuliaDir)
	}
}

// sharedJuliaCwd returns a stable temp dir so display/eval/trace tests that
// don't mutate conflicting state reuse one warm Julia session on the shared
// daemon. Tests that kill/interrupt/restart must use their own cwd or label.
func sharedJuliaCwd(t *testing.T) string {
	t.Helper()
	sharedJuliaOnce.Do(func() {
		dir, err := os.MkdirTemp("", "repld-julia-shared-")
		require.NoError(t, err)
		sharedJuliaDir = dir
	})
	return sharedJuliaDir
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

// startTestDaemon launches serveDaemon in a goroutine. A nil t means the daemon
// is not bound to any test's lifetime (shared daemon, torn down via TestMain):
// it skips t.Cleanup and panics rather than t.Fatal on startup failure.
func startTestDaemon(t *testing.T) (socketPath string, stop func(), wg *sync.WaitGroup) {
	if t != nil {
		t.Helper()
	}
	// Keep the AF_UNIX path short (macOS caps it near 104 chars); /tmp is short
	// on Unix, but doesn't exist on Windows, so fall back to the OS temp dir.
	base := "/tmp"
	if runtime.GOOS == "windows" {
		base = os.TempDir()
	}
	socketDir, err := os.MkdirTemp(base, "repld-test-")
	if err != nil {
		panic(err)
	}
	if t != nil {
		t.Cleanup(func() { os.RemoveAll(socketDir) })
	}
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
		if t == nil {
			os.RemoveAll(socketDir)
		}
	}
	return
}

func waitForSocket(t *testing.T, socketPath string, errCh <-chan error) {
	if t != nil {
		t.Helper()
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-errCh:
			if t != nil {
				require.NoError(t, err)
			} else if err != nil {
				panic(err)
			}
		default:
		}
		if _, err := os.Stat(socketPath); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	if t != nil {
		require.Fail(t, "daemon socket did not appear in time")
	} else {
		panic("shared daemon socket did not appear in time")
	}
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

func TestExecuteRawWithoutControlDoesNotPanic(t *testing.T) {
	sess := newSession(julia.Adapter{}, "SENTINEL", nil, nil)
	sess.stdin = &nopWriteCloser{}
	sess.stdout = bufio.NewReader(strings.NewReader("SENTINEL\n"))
	sess.stderr = bufio.NewReader(strings.NewReader("SENTINEL\n"))

	_, err := sess.executeRaw("1", nil, true, 1)
	require.Error(t, err)
	require.Contains(t, err.Error(), "control")
}
