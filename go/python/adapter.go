// Python adapter driving a `python -u -q -i` subprocess (-u for unbuffered output)
package python

import (
	_ "embed"
	"encoding/hex"
	"fmt"
	"runtime"
	"strings"
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
	args := append([]string{"-u", "-q", "-i"}, forwarded...)
	return append(args, "-c", bootstrapExec())
}

func bootstrapExec() string {
	return fmt.Sprintf(`exec(bytes.fromhex("%s").decode())`, hex.EncodeToString([]byte(runtimeSource)))
}

func (Adapter) BootstrapStmt() string { return "" }

func (Adapter) WrapEval(hexCode string, _ bool) string {
	return fmt.Sprintf(`_repld_run("%s")`, hexCode)
}

func (Adapter) EvalFileStmt(path string, args []string) string {
	argv := append([]string{path}, args...)
	elems := make([]string, len(argv))
	for i, a := range argv {
		elems[i] = fmt.Sprintf(`bytes.fromhex("%s").decode("utf-8")`, hex.EncodeToString([]byte(a)))
	}
	return fmt.Sprintf(`import sys as _sys
_repld_fp = [%s]
_repld_saved_argv = _sys.argv
_sys.argv = _repld_fp
try:
    exec(compile(open(_repld_fp[0], encoding="utf-8").read(), _repld_fp[0], "exec"), _NS)
finally:
    _sys.argv = _repld_saved_argv
    del _repld_fp, _repld_saved_argv`, strings.Join(elems, ", "))
}

func (Adapter) SentinelStmt(sentinel string) string {
	return fmt.Sprintf(`import sys as _s; _ = _s.stderr.write("%s\n"); _s.stderr.flush(); _ = _s.stdout.write("%s\n"); _s.stdout.flush()`, sentinel, sentinel)
}

func (Adapter) InterruptViaControl() bool { return true }
