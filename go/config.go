package main

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/Beforerr/repld/go/julia"
	"github.com/Beforerr/repld/go/python"
	"github.com/Beforerr/repld/go/r"
	"github.com/Beforerr/repld/go/wolfram"
)

type langConfig struct {
	displayName string
	evalFlags   []string
	printFlags  []string
	adapter     Adapter
}

var langs = map[string]langConfig{
	"julia": {
		displayName: "Julia",
		evalFlags:   []string{"e", "eval"},
		printFlags:  []string{"E", "print"},
		adapter:     julia.Adapter{},
	},
	"python": {
		displayName: "Python",
		evalFlags:   []string{"c", "command"},
		adapter:     python.Adapter{},
	},
	"r": {
		displayName: "R",
		evalFlags:   []string{"e", "eval"},
		adapter:     r.Adapter{},
	},
	"wolfram": {
		displayName: "Wolfram",
		printFlags: []string{"c", "code"},
		adapter:    wolfram.Adapter{},
	},
}

// Unknown lang means label-only reuse; accept both eval spellings for parse.
func evalPrintFlags(lang string) (eval, print []string) {
	if lc, ok := langs[lang]; ok {
		return lc.evalFlags, lc.printFlags
	}
	return []string{"e", "eval", "c", "command"}, []string{"E", "print"}
}

func langForExe(exe string) string {
	base := strings.ToLower(filepath.Base(exe))
	switch {
	case strings.Contains(base, "python"):
		return "python"
	case strings.Contains(base, "julia"):
		return "julia"
	case base == "r" || base == "r.exe":
		return "r"
	case base == "wolframscript" || base == "wolframscript.exe" || base == "wolfram" || base == "wolfram.exe" || strings.Contains(base, "wolframkernel") || strings.Contains(base, "mathkernel"):
		return "wolfram"
	}
	return ""
}

func adapterFor(lang string) Adapter { return langs[lang].adapter }

func defaultSocketPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.TempDir()
	}
	return filepath.Join(home, ".local", "share", "repld", "daemon.sock")
}
