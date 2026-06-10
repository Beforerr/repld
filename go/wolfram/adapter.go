package wolfram

import (
	_ "embed"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
)

//go:embed runtime.wl
var runtimeSource string

// wolframscript reads the script at launch, so one file serves every session;
// per-launch files would leak, since nothing tracks when wolframscript is done
// with them. Rewritten if missing: tmp cleaners can remove it under a
// long-lived daemon.
var script struct {
	sync.Mutex
	path string
}

func runtimeScript() (string, error) {
	script.Lock()
	defer script.Unlock()
	if script.path != "" {
		if _, err := os.Stat(script.path); err == nil {
			return script.path, nil
		}
	}
	f, err := os.CreateTemp("", "repld-wolfram-runtime-*.wls")
	if err != nil {
		return "", fmt.Errorf("create Wolfram runtime script: %w", err)
	}
	_, err = f.WriteString(strings.ReplaceAll(strings.ReplaceAll(runtimeSource, "\r\n", "\n"), "\r", "\n"))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("write Wolfram runtime script: %w", err)
	}
	script.path = f.Name()
	return script.path, nil
}

type Adapter struct{}

func (Adapter) DefaultExe() string { return "wolframscript" }

func (Adapter) LaunchArgs(forwarded []string) ([]string, error) {
	path, err := runtimeScript()
	if err != nil {
		return nil, err
	}
	args := append([]string{}, forwarded...)
	return append(args, "-script", path), nil
}

func (Adapter) BootstrapStmt() string { return "" }

func (Adapter) WrapEval(hexCode string, printResult bool) string {
	return fmt.Sprintf("REPLD_EVAL %s %s", hexCode, wlBool(printResult))
}

// Block successfully scopes $ScriptCommandLine
func (Adapter) EvalFileStmt(path string, args []string) string {
	elems := make([]string, 0, len(args)+1)
	elems = append(elems, fmt.Sprintf(`repldDecode["%s"]`, hex.EncodeToString([]byte(path))))
	for _, a := range args {
		elems = append(elems, fmt.Sprintf(`repldDecode["%s"]`, hex.EncodeToString([]byte(a))))
	}
	return fmt.Sprintf(`Block[{$ScriptCommandLine = {%s}}, Get[First[$ScriptCommandLine]]]`,
		strings.Join(elems, ", "))
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
