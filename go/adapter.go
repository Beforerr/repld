package main

// Adapter is the language-specific runtime boundary.
type Adapter interface {
	DefaultExe() string

	LaunchArgs(forwarded []string) []string

	// BootstrapStmt loads the embedded runtime.
	BootstrapStmt() string
	WrapEval(hexCode string, printResult bool) string

	// EvalFileStmt evals a source file (restoring argv after eval).
	EvalFileStmt(path string, args []string) string

	// SentinelStmt is the stdout/stderr drain barrier.
	SentinelStmt(sentinel string) string

	// InterruptViaControl reports whether the runtime listens on the control socket for interrupts.
	// If false, the runtime is interrupted via process SIGINT.
	InterruptViaControl() bool
}
