// Python adapter driving a `python -u -q -i` subprocess (-u for unbuffered output)
package python

import (
	_ "embed"
	"fmt"
	"runtime"
)

//go:embed runtime.py
var runtimeSource string

type Adapter struct{}

func (Adapter) DefaultExe() string {
	if runtime.GOOS == "windows" {
		return "python"
	}
	return "python3"
}

func (Adapter) LaunchArgs(forwarded []string) []string {
	return append([]string{"-u", "-q", "-i"}, forwarded...)
}

// SessionKey: Python has no project notion; the interpreter path is the env.
func (a Adapter) SessionKey(exe string, _ []string) string {
	if exe == "" {
		return a.DefaultExe()
	}
	return exe
}

func (Adapter) RuntimeSource() string { return runtimeSource }

func (Adapter) LoadRuntimeStmt(hexSource string) string {
	return fmt.Sprintf(`exec(bytes.fromhex("%s").decode())`, hexSource)
}

func (Adapter) WrapEval(hexCode string, _ bool) string {
	return fmt.Sprintf(`_repld_run("%s")`, hexCode)
}

func (Adapter) SentinelStmt(sentinel string) string {
	// Assign write() results to _ so the interactive interpreter never echoes the
	// char counts (the no-op displayhook isn't set yet during startup drain).
	return fmt.Sprintf(`import sys as _s; _ = _s.stderr.write("%s\n"); _s.stderr.flush(); _ = _s.stdout.write("%s\n"); _s.stdout.flush()`, sentinel, sentinel)
}

func (Adapter) InterruptViaControl() bool { return true }
