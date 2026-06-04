package r

import (
	_ "embed"
	"fmt"
	"strconv"
	"strings"
)

//go:embed runtime.R
var runtimeSource string

type Adapter struct{}

func (Adapter) DefaultExe() string { return "R" }

func (Adapter) LaunchArgs(forwarded []string) []string {
	return append([]string{"--slave", "--no-save", "--no-restore"}, forwarded...)
}

func (a Adapter) SessionKey(exe string, _ []string) string {
	if exe == "" {
		return a.DefaultExe()
	}
	return exe
}

func (Adapter) RuntimeSource() string { return runtimeSource }

func (Adapter) LoadRuntimeStmt(_ string) string {
	source := strings.ReplaceAll(strings.ReplaceAll(runtimeSource, "\r\n", "\n"), "\r", "\n")
	return fmt.Sprintf(`eval(parse(text = %s), envir = .GlobalEnv)`, strconv.Quote(source))
}

func (Adapter) WrapEval(hexCode string, printResult bool) string {
	return fmt.Sprintf(`.repld_run("%s", %s)`, hexCode, rBool(printResult))
}

func (Adapter) SentinelStmt(sentinel string) string {
	return fmt.Sprintf(`cat("%s\n", file=stderr()); flush(stderr()); cat("%s\n"); flush(stdout())`, sentinel, sentinel)
}

func rBool(b bool) string {
	if b {
		return "TRUE"
	}
	return "FALSE"
}
