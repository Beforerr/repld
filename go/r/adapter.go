package r

import (
	_ "embed"
	"encoding/hex"
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

func (Adapter) BootstrapStmt() string {
	source := strings.ReplaceAll(strings.ReplaceAll(runtimeSource, "\r\n", "\n"), "\r", "\n")
	return fmt.Sprintf(`eval(parse(text = %s), envir = .GlobalEnv)`, strconv.Quote(source))
}

func (Adapter) WrapEval(hexCode string, printResult bool) string {
	return fmt.Sprintf(`.repld_run("%s", %s)`, hexCode, rBool(printResult))
}

// EvalFileStmt: R has no per-eval argv, so a commandArgs shadow in .GlobalEnv
// (top-level lookup hits it before base) serves the args
func (Adapter) EvalFileStmt(path string, args []string) string {
	elems := make([]string, len(args))
	for i, a := range args {
		elems[i] = fmt.Sprintf(`.repld_decode("%s")`, hex.EncodeToString([]byte(a)))
	}
	return fmt.Sprintf(`{
  .repld_fa <- c(%s)
  .repld_had <- exists("commandArgs", envir = .GlobalEnv, inherits = FALSE)
  .repld_prev <- if (.repld_had) get("commandArgs", envir = .GlobalEnv) else NULL
  assign("commandArgs", function(trailingOnly = FALSE) {
    if (isTRUE(trailingOnly)) .repld_fa else c("R", "--args", .repld_fa)
  }, envir = .GlobalEnv)
  on.exit({
    if (.repld_had) assign("commandArgs", .repld_prev, envir = .GlobalEnv)
    else if (exists("commandArgs", envir = .GlobalEnv, inherits = FALSE)) rm("commandArgs", envir = .GlobalEnv)
  })
  source(.repld_decode("%s"), local = FALSE, echo = FALSE)
}`, strings.Join(elems, ", "), hex.EncodeToString([]byte(path)))
}

func (Adapter) SentinelStmt(sentinel string) string {
	return fmt.Sprintf(`cat("%s\n", file=stderr()); flush(stderr()); cat("%s\n"); flush(stdout())`, sentinel, sentinel)
}

// InterruptViaControl is false: the R runtime does not read the control socket
// for interrupts. The engine delivers SIGINT, caught by .repld_run's interrupt
// handler (R survives SIGINT in non-interactive --slave mode).
func (Adapter) InterruptViaControl() bool { return false }

func rBool(b bool) string {
	if b {
		return "TRUE"
	}
	return "FALSE"
}
