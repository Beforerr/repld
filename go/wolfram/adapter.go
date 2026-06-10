package wolfram

import (
	_ "embed"
	"fmt"
	"os"
	"strconv"
	"strings"
)

//go:embed runtime.wl
var runtimeSource string

type Adapter struct{}

func (Adapter) DefaultExe() string { return "wolframscript" }

func (Adapter) LaunchArgs(forwarded []string) []string {
	source := strings.ReplaceAll(strings.ReplaceAll(runtimeSource, "\r\n", "\n"), "\r", "\n")
	f, err := os.CreateTemp("", "repld-wolfram-*.wls")
	if err != nil {
		return []string{"-code", `Print["repld: failed to create Wolfram runtime script"]`}
	}
	_, _ = f.WriteString(source)
	_ = f.Close()
	args := append([]string{}, forwarded...)
	return append(args, "-script", f.Name())
}

func (a Adapter) SessionKey(exe string, _ []string) string {
	if exe == "" {
		return a.DefaultExe()
	}
	return exe
}

func (Adapter) BootstrapStmt() string { return "" }

func (Adapter) WrapEval(hexCode string, printResult bool) string {
	return fmt.Sprintf("REPLD_EVAL %s %s", hexCode, wlBool(printResult))
}

func (Adapter) SentinelStmt(sentinel string) string {
	return "REPLD_SENT " + strconv.Quote(sentinel)
}

// InterruptViaControl is false. wolframscript proxies to a kernel that does not
// usefully abort a running evaluation on SIGINT, so interrupt = session kill.
func (Adapter) InterruptViaControl() bool { return false }

func wlBool(b bool) string {
	if b {
		return "True"
	}
	return "False"
}
