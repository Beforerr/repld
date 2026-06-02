package main

// Adapter isolates the language-specific parts of driving an interpreter; the
// engine is otherwise language-agnostic.
type Adapter interface {
	DefaultExe() string // looked up in PATH when no explicit exe is passed

	// LaunchArgs builds the interpreter argv (without the exe).
	LaunchArgs(projectVal string, forwarded []string) []string

	// RuntimeSource is injected at startup and must implement the control-socket
	// contract: dial back, authenticate with the token, write one status frame
	// per eval, listen for interrupt bytes. See julia_client_runtime.jl.
	RuntimeSource() string
	LoadRuntimeStmt(hexSource string) string          // statement that loads the hex'd runtime
	WrapEval(hexCode string, printResult bool) string // runs hex'd code, emits the control frame

	// SentinelStmt prints sentinel to stderr then stdout (flushing each); the
	// engine uses it as the per-eval drain barrier.
	SentinelStmt(sentinel string) string
}
