package main

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
)

type daemonState struct {
	manager     *SessionManager
	lastRequest atomic.Int64 // UnixNano
	stopOnce    sync.Once
	stopCh      chan struct{}
}

func handleRequest(state *daemonState, req protocolRequest) response {
	state.lastRequest.Store(time.Now().UnixNano())

	switch req.Action {
	case "trace":
		key, kerr := state.manager.targetKey(req.ID, req.Lang, req.Session, req.Cwd, req.Exe)
		if kerr != nil {
			return errResp(kerr.Error())
		}
		err := state.manager.lastError(key)
		if err == nil {
			return errResp("No saved traceback for this session.")
		}
		return response{Output: formatTraceOutput(err, req.TraceLevel)}

	case "sessions":
		items := state.manager.list()
		if len(items) == 0 {
			return response{Output: "No active sessions."}
		}
		out, err := formatSessions(items)
		if err != nil {
			return errResp(err.Error())
		}
		return response{Output: out}

	case "interrupt":
		key, kerr := state.manager.targetKey(req.ID, req.Lang, req.Session, req.Cwd, req.Exe)
		if kerr != nil {
			return errResp(kerr.Error())
		}
		msg, err := state.manager.interrupt(key, 3.0)
		if err != nil {
			return errResp(err.Error())
		}
		return response{Output: msg}

	case "close":
		key, kerr := state.manager.targetKey(req.ID, req.Lang, req.Session, req.Cwd, req.Exe)
		if kerr != nil {
			return errResp(kerr.Error())
		}
		msg, err := state.manager.close(key)
		if err != nil {
			return errResp(err.Error())
		}
		return response{Output: msg}

	case "free":
		key, kerr := state.manager.targetKey(req.ID, req.Lang, req.Session, req.Cwd, req.Exe)
		if kerr != nil {
			return errResp(kerr.Error())
		}
		msg, err := state.manager.free(key)
		if err != nil {
			return errResp(err.Error())
		}
		return response{Output: msg}

	case "stop":
		state.stopOnce.Do(func() { close(state.stopCh) })
		return response{Output: "Daemon stopping."}

	case "ping":
		return response{Output: "pong"}

	default:
		return errResp(fmt.Sprintf("Unknown action: %q", req.Action))
	}
}

// formatSessions renders sessions as a YAML sequence with one flow-style
// mapping per line: compact and scannable, yet a single parseable document.
func formatSessions(items []sessionInfo) (string, error) {
	sort.Slice(items, func(i, j int) bool {
		return items[i].Lang+items[i].Session+items[i].Dir <
			items[j].Lang+items[j].Session+items[j].Dir
	})
	var root yaml.Node
	if err := root.Encode(items); err != nil {
		return "", err
	}
	for _, item := range root.Content {
		item.Style = yaml.FlowStyle
	}
	b, err := yaml.Marshal(&root)
	return string(b), err
}

func errResp(msg string) response {
	return response{Error: msg}
}

func normalizedTraceLevel(level string) string {
	switch strings.ToLower(level) {
	case "short", "compact":
		return "short"
	case "", "smart", "default":
		return "smart"
	case "full", "long", "verbose":
		return "full"
	default:
		return "smart"
	}
}

func formatError(err *evalError, level, sessionID string) string {
	traceHint := strings.TrimSpace(err.smart) != strings.TrimSpace(err.short)
	hint := fmt.Sprintf("Trace saved: `repld trace %s`.", sessionID)
	switch normalizedTraceLevel(level) {
	case "short":
		if !traceHint {
			return err.short
		}
		return err.short + "\n\n" + hint
	case "full":
		return err.full
	default:
		if !traceHint {
			return err.short
		}
		return err.smart + hint
	}
}

func formatTraceOutput(err *evalError, level string) string {
	level = cmp.Or(level, "full")
	switch normalizedTraceLevel(level) {
	case "short":
		return err.short + "\n"
	case "full":
		return err.full + "\n"
	default:
		return err.smart
	}
}

func handleConn(conn net.Conn, state *daemonState) {
	defer conn.Close()

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024)
	if !scanner.Scan() {
		return
	}

	var req protocolRequest
	if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
		json.NewEncoder(conn).Encode(errResp(fmt.Sprintf("invalid JSON: %v", err)))
		return
	}

	if req.Action == "eval" {
		handleStreamingEval(state, req, conn)
		return
	}
	json.NewEncoder(conn).Encode(handleRequest(state, req))
}

func handleStreamingEval(state *daemonState, req protocolRequest, conn net.Conn) {
	state.lastRequest.Store(time.Now().UnixNano())
	enc := json.NewEncoder(conn)
	emit := func(f streamFrame) { _ = enc.Encode(f) }

	if req.RequireExisting {
		if req.Fresh || !state.manager.hasLiveSession(req.Lang, req.Session, req.Cwd, req.Exe) {
			emit(streamFrame{Done: true, Error: "no existing session for label; pass an interpreter or --lang to create one"})
			return
		}
	}
	if req.Fresh {
		state.manager.restart(req.Lang, req.Session, req.Cwd, req.Exe)
	}
	sess, err := state.manager.getOrCreate(req.Lang, req.Cwd, req.Session, req.Exe, req.Args, req.OwnerPID, req.OwnerStart)
	if err != nil {
		emit(streamFrame{Done: true, Error: err.Error()})
		return
	}
	for _, chunk := range sess.drainStartup() {
		if chunk.isStderr {
			emit(streamFrame{Stderr: chunk.data})
		} else {
			emit(streamFrame{Chunk: chunk.data})
		}
	}

	// Tie eval to connection: cancelling ctx on client disconnect lets execute interrupt it
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		buf := make([]byte, 256)
		for {
			if _, rerr := conn.Read(buf); rerr != nil {
				cancel()
				return
			}
		}
	}()

	onChunk := func(data string, isStderr bool) {
		if isStderr {
			emit(streamFrame{Stderr: data})
		} else {
			emit(streamFrame{Chunk: data})
		}
	}
	// File mode: the adapter's snippet is just code through the normal eval path.
	code, printResult := req.Code, req.PrintResult
	if req.File != "" {
		code = sess.adapter.EvalFileStmt(req.File, req.FileArgs)
		printResult = false
	}
	err = sess.execute(ctx, code, printResult, onChunk)
	if err != nil {
		if !sess.isAlive() {
			state.manager.remove(req.Lang, req.Session, req.Cwd, req.Exe)
		}
		if evalErr, ok := err.(*evalError); ok {
			state.manager.recordError(req.Lang, req.Session, req.Cwd, req.Exe, evalErr)
			emit(streamFrame{Done: true, Error: formatError(evalErr, req.TraceLevel, sess.id)})
			return
		}
		emit(streamFrame{Done: true, Error: err.Error()})
		return
	}
	emit(streamFrame{Done: true})
}

func pingDaemon(socketPath string) bool {
	conn, err := net.DialTimeout("unix", socketPath, time.Second)
	if err != nil {
		return false
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	if err := json.NewEncoder(conn).Encode(protocolRequest{Action: "ping"}); err != nil {
		return false
	}
	var resp response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return false
	}
	return resp.Output == "pong"
}

// listenExclusive binds the socket without stealing a live daemon's
func listenExclusive(socketPath string) (net.Listener, error) {
	for attempt := range 3 {
		ln, err := net.Listen("unix", socketPath)
		if err == nil {
			return ln, nil
		}
		if pingDaemon(socketPath) {
			return nil, fmt.Errorf("another repld daemon is already running on %s", socketPath)
		}
		// Stale socket from a crashed daemon (or a non-socket file): drop it and retry.
		if attempt < 2 {
			os.Remove(socketPath)
		}
	}
	return nil, fmt.Errorf("could not bind repld socket %s (raced with another daemon)", socketPath)
}

func serveDaemon(socketPath string, idleTimeout time.Duration) error {
	dir := filepath.Dir(socketPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	_ = os.Chmod(dir, 0700)

	ln, err := listenExclusive(socketPath)
	if err != nil {
		return err
	}
	_ = os.Chmod(socketPath, 0600)

	pidPath := filepath.Join(filepath.Dir(socketPath), "daemon.pid")
	os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0644)
	fmt.Fprintf(os.Stderr, "repld listening on %s\n", socketPath)

	state := &daemonState{
		manager: newSessionManager(),
		stopCh:  make(chan struct{}),
	}
	state.lastRequest.Store(time.Now().UnixNano())

	go state.manager.reapLoop(state.stopCh)

	// Idle watchdog: closes listener when idle or stop requested
	go func() {
		defer ln.Close()
		if idleTimeout <= 0 {
			<-state.stopCh
			return
		}
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-state.stopCh:
				return
			case <-ticker.C:
				if time.Since(time.Unix(0, state.lastRequest.Load())) > idleTimeout {
					return
				}
			}
		}
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			break
		}
		go handleConn(conn, state)
	}

	state.manager.shutdown()
	os.Remove(socketPath)
	os.Remove(pidPath)
	return nil
}
