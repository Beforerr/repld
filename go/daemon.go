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
		err := state.manager.lastError(req.Lang, req.Session, req.Cwd, discFor(req))
		if err == nil {
			return errResp("No saved traceback for this session.")
		}
		return response{Output: formatTraceOutput(err, req.TraceLevel)}

	case "sessions":
		sessions := state.manager.list()
		if len(sessions) == 0 {
			return response{Output: "No active sessions."}
		}
		sort.Slice(sessions, func(i, j int) bool {
			return sessions[i].lang+sessions[i].label < sessions[j].lang+sessions[j].label
		})
		lines := []string{"Active sessions:"}
		for _, s := range sessions {
			label := s.label
			if strings.HasPrefix(label, "~") {
				label = "session " + strings.TrimPrefix(label, "~")
			} else {
				label = "dir " + label
			}
			line := fmt.Sprintf("  [%s] %s", s.lang, label)
			if s.project != "" {
				line += " project=" + s.project
			}
			if !s.alive {
				line += " status=dead"
			} else if s.busyFor > 0 {
				line += fmt.Sprintf(" busy=%.1fs", s.busyFor.Seconds())
			}
			if len(s.args) > 0 {
				line += " args=" + strings.Join(s.args, " ")
			}
			if s.logFile != "" {
				line += " log=" + s.logFile
			}
			lines = append(lines, line)
		}
		return response{Output: strings.Join(lines, "\n")}

	case "interrupt":
		msg, err := state.manager.interrupt(req.Lang, req.Session, req.Cwd, discFor(req), 3.0)
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

func errResp(msg string) response {
	return response{Error: msg}
}

// discFor is the session's environment discriminant, or "" when the language is
// unknown (label-only reuse, where the discriminant isn't part of the key).
func discFor(req protocolRequest) string {
	if a := adapterFor(req.Lang); a != nil {
		return a.SessionKey(req.Exe, req.Args)
	}
	return ""
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

func formatError(err *evalError, level string) string {
	traceHint := strings.TrimSpace(err.smart) != strings.TrimSpace(err.short)
	switch normalizedTraceLevel(level) {
	case "short":
		if !traceHint {
			return err.short
		}
		return err.short + "\n\nTrace saved: run `trace --trace [smart|full]` to inspect"
	case "full":
		return err.full
	default:
		if !traceHint {
			return err.short
		}
		return err.smart + "Trace saved: run `trace` to inspect"
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

	disc := discFor(req)
	if req.RequireExisting {
		if req.Fresh || !state.manager.hasLiveSession(req.Lang, req.Session, req.Cwd, disc) {
			emit(streamFrame{Done: true, Error: "no existing session for label; pass an interpreter or --lang to create one"})
			return
		}
	}
	if req.Fresh {
		state.manager.restart(req.Lang, req.Session, req.Cwd, disc)
	}
	sess, err := state.manager.getOrCreate(req.Lang, req.Cwd, req.Session, req.Exe, req.Args)
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
			state.manager.remove(req.Lang, req.Session, req.Cwd, disc)
		}
		if evalErr, ok := err.(*evalError); ok {
			state.manager.recordError(req.Lang, req.Session, req.Cwd, disc, evalErr)
			emit(streamFrame{Done: true, Error: formatError(evalErr, req.TraceLevel)})
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
