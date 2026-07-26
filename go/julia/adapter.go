// Julia adapter driving a `julia -i --project=<dir>` subprocess
package julia

import (
	_ "embed"
	"encoding/hex"
	"fmt"
	"strings"
)

//go:embed runtime.jl
var runtimeSource string

type Adapter struct{}

func (Adapter) DefaultExe() string { return "julia" }

func (Adapter) LaunchArgs(forwarded []string) []string {
	var args []string
	rest := forwarded
	// A juliaup channel (+x) must be first argument.
	if len(rest) > 0 && strings.HasPrefix(rest[0], "+") {
		args = append(args, rest[0])
		rest = rest[1:]
	}
	args = append(args, "-i")
	if !hasProject(forwarded) {
		args = append(args, "--project=@.")
	}
	args = append(args, rest...)
	return args
}

func hasProject(args []string) bool {
	return projectOf(args) != ""
}

func projectOf(args []string) string {
	for i, a := range args {
		if v, ok := strings.CutPrefix(a, "--project="); ok {
			return v
		}
		if a == "--project" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func (Adapter) BootstrapStmt() string {
	return fmt.Sprintf(`include_string(Main, String(hex2bytes("%s")), "repld runtime")`, hex.EncodeToString([]byte(runtimeSource)))
}

func (Adapter) WrapEval(hexCode string, printResult bool) string {
	return fmt.Sprintf(`Main.JuliaClientRuntime.run("%s", %t)`, hexCode, printResult)
}

func (Adapter) EvalFileStmt(path string, args []string) string {
	hexPath := hex.EncodeToString([]byte(path))
	argElems := make([]string, len(args))
	for i, a := range args {
		argElems[i] = fmt.Sprintf(`String(hex2bytes("%s"))`, hex.EncodeToString([]byte(a)))
	}
	return fmt.Sprintf(`let _p = String(hex2bytes("%s")), _old = copy(ARGS), _oldpf = Base.PROGRAM_FILE
    empty!(ARGS); append!(ARGS, AbstractString[%s])
    Base.PROGRAM_FILE = _p
    try
        Base.include(Main, _p)
    finally
        empty!(ARGS); append!(ARGS, _old)
        Base.PROGRAM_FILE = _oldpf
    end
end`, hexPath, strings.Join(argElems, ", "))
}

func (Adapter) SentinelStmt(sentinel string) string {
	return fmt.Sprintf(`flush(stderr); println(stderr, "%s"); flush(stderr); println(stdout, "%s"); flush(stdout)`, sentinel, sentinel)
}

func (Adapter) InterruptViaControl() bool { return true }
