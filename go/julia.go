package main

import (
	_ "embed"
	"fmt"
	"strings"
)

//go:embed julia_client_runtime.jl
var juliaClientRuntime string

type juliaAdapter struct{}

func (juliaAdapter) DefaultExe() string { return "julia" }

func (juliaAdapter) LaunchArgs(projectVal string, forwarded []string) []string {
	var args []string
	rest := forwarded
	// A juliaup channel (+x) must be Julia's very first argument.
	if len(rest) > 0 && strings.HasPrefix(rest[0], "+") {
		args = append(args, rest[0])
		rest = rest[1:]
	}
	args = append(args, "-i")
	args = append(args, rest...)
	args = append(args, fmt.Sprintf("--project=%s", projectVal))
	return args
}

func (juliaAdapter) RuntimeSource() string { return juliaClientRuntime }

func (juliaAdapter) LoadRuntimeStmt(hexSource string) string {
	return fmt.Sprintf(`include_string(Main, String(hex2bytes("%s")), "julia-client runtime")`, hexSource)
}

func (juliaAdapter) WrapEval(hexCode string, printResult bool) string {
	return fmt.Sprintf(`Main.JuliaClientRuntime.run("%s", %t)`, hexCode, printResult)
}

func (juliaAdapter) SentinelStmt(sentinel string) string {
	return fmt.Sprintf(`flush(stderr); println(stderr, "%s"); flush(stderr); println(stdout, "%s"); flush(stdout)`, sentinel, sentinel)
}
