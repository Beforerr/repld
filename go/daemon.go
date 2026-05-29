package main

import (
	"bufio"
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
		err := state.manager.lastError(req.Session, req.Project, req.Cwd)
		if err == nil {
			return errResp("No saved Julia traceback for this session.")
		}
		return response{Output: formatTraceOutput(err, req.TraceLevel)}

	case "sessions":
		sessions := state.manager.list()
		if len(sessions) == 0 {
			return response{Output: "No active Julia sessions."}
		}
		sort.Slice(sessions, func(i, j int) bool {
			return sessions[i].key < sessions[j].key
		})
		lines := []string{"Active Julia sessions:"}
		for _, s := range sessions {
			label := s.key
			if strings.HasPrefix(label, "~") {
				label = "session " + strings.TrimPrefix(label, "~")
			} else {
				label = "project " + label
			}
			line := "  " + label
			if s.project != "" && s.project != s.key {
				line += " project=" + s.project
			}
			if !s.alive {
				line += " status=dead"
			} else if s.busyFor > 0 {
				line += fmt.Sprintf(" busy=%.1fs", s.busyFor.Seconds())
			}
			if s.juliaCmd != "" {
				line += " julia_cmd=" + s.juliaCmd
			}
			if s.logFile != "" {
				line += " log=" + s.logFile
			}
			lines = append(lines, line)
		}
		return response{Output: strings.Join(lines, "\n")}

	case "interrupt":
		msg, err := state.manager.interrupt(req.Session, req.Project, req.Cwd, 3.0)
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

func formatJuliaError(err *juliaEvalError, level string) string {
	traceHint := strings.TrimSpace(err.smart) != strings.TrimSpace(err.short)
	switch normalizedTraceLevel(level) {
	case "short":
		if !traceHint {
			return err.short
		}
		return err.short + "\n\nTrace saved: run `julia-client trace --trace [smart|full]` to inspect"
	case "full":
		return err.full
	default:
		if !traceHint {
			return err.short
		}
		return err.smart + "Trace saved: run `julia-client trace` to inspect"
	}
}

func formatTraceOutput(err *juliaEvalError, level string) string {
	if level == "" {
		level = "full"
	}
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

	if req.Fresh {
		state.manager.restart(req.Session, req.Project, req.Cwd)
	}
	sess, err := state.manager.getOrCreate(req.Cwd, req.Project, req.Session, req.JuliaCmd)
	if err != nil {
		emit(streamFrame{Done: true, Error: err.Error()})
		return
	}

	// Watch for client disconnect. If the client goes away mid-eval (e.g.
	// `timeout 30 julia-client` kills it), interrupt the session so the
	// computation stops instead of orphaning and holding the session lock.
	// The client never writes after its request, so a Read here blocks until
	// the connection closes.
	evalDone := make(chan struct{})
	go func() {
		buf := make([]byte, 256)
		for {
			if _, rerr := conn.Read(buf); rerr != nil {
				select {
				case <-evalDone: // eval already finished; normal close
				default:
					sess.interrupt(3.0)
				}
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
	err = sess.execute(req.Code, req.PrintResult, onChunk)
	close(evalDone)
	if err != nil {
		if !sess.isAlive() {
			state.manager.remove(req.Session, req.Project, req.Cwd)
		}
		if juliaErr, ok := err.(*juliaEvalError); ok {
			state.manager.recordError(req.Session, req.Project, req.Cwd, juliaErr)
			emit(streamFrame{Done: true, Error: formatJuliaError(juliaErr, req.TraceLevel)})
			return
		}
		emit(streamFrame{Done: true, Error: err.Error()})
		return
	}
	emit(streamFrame{Done: true})
}

func serveDaemon(socketPath string, idleTimeout time.Duration) error {
	if err := os.MkdirAll(filepath.Dir(socketPath), 0755); err != nil {
		return err
	}
	os.Remove(socketPath)

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return err
	}

	pidPath := filepath.Join(filepath.Dir(socketPath), "daemon.pid")
	os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0644)
	fmt.Fprintf(os.Stderr, "julia-daemon listening on %s\n", socketPath)

	state := &daemonState{
		manager: newSessionManager(),
		stopCh:  make(chan struct{}),
	}
	state.lastRequest.Store(time.Now().UnixNano())

	// Idle watchdog: closes listener when idle or stop requested
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-state.stopCh:
				ln.Close()
				return
			case <-ticker.C:
				idle := time.Since(time.Unix(0, state.lastRequest.Load()))
				if idle > idleTimeout {
					ln.Close()
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
