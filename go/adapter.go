package main

// Adapter is the language-specific runtime boundary.
type Adapter interface {
	DefaultExe() string

	LaunchArgs(forwarded []string) []string

	// SessionKey separates environments: project, interpreter, or runtime identity.
	SessionKey(exe string, forwarded []string) string

	RuntimeSource() string
	LoadRuntimeStmt(hexSource string) string
	WrapEval(hexCode string, printResult bool) string

	// SentinelStmt is the stdout/stderr drain barrier.
	SentinelStmt(sentinel string) string
}
