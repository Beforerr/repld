package main

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/Beforerr/repld/go/julia"
	"github.com/Beforerr/repld/go/python"
	"github.com/Beforerr/repld/go/r"
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
	}
	return ""
}

func adapterFor(lang string) Adapter { return langs[lang].adapter }

func defaultSocketPath() string {
	return filepath.Join(os.Getenv("HOME"), ".local", "share", "repld", "daemon.sock")
}
