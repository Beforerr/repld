// Julia adapter driving a `julia -i --project=<dir>` subprocess
package julia

import (
	_ "embed"
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

// each project is its own session.
func (Adapter) SessionKey(_ string, forwarded []string) string {
	if p := projectOf(forwarded); p != "" {
		return p
	}
	return "@."
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

func (Adapter) RuntimeSource() string { return runtimeSource }

func (Adapter) LoadRuntimeStmt(hexSource string) string {
	return fmt.Sprintf(`include_string(Main, String(hex2bytes("%s")), "repld runtime")`, hexSource)
}

func (Adapter) WrapEval(hexCode string, printResult bool) string {
	return fmt.Sprintf(`Main.JuliaClientRuntime.run("%s", %t)`, hexCode, printResult)
}

func (Adapter) SentinelStmt(sentinel string) string {
	return fmt.Sprintf(`flush(stderr); println(stderr, "%s"); flush(stderr); println(stdout, "%s"); flush(stdout)`, sentinel, sentinel)
}
