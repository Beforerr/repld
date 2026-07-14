package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func ownerDead(pid int, startTime int64) bool {
	if pid <= 0 {
		return false
	}
	st, alive := procInfo(pid)
	if !alive {
		return true
	}
	return startTime != 0 && st != 0 && st != startTime
}

func resolveOwner(explicit string) (int, int64) {
	if explicit != "" {
		return ownerFrom(explicit)
	}
	if env := os.Getenv("REPLD_OWNER_PID"); env != "" {
		return ownerFrom(env)
	}
	if os.Getenv("CLAUDECODE") != "" {
		// The immediate parent is the per-call shell; the harness sits some
		// variable number of levels up (Claude Code wraps Bash as a compound
		// `zsh -c`, and users add timeout/env/xargs wrappers), so walk the
		// ancestry rather than assuming a fixed depth.
		if h := harnessPID(os.Getppid(), ppidOf, procIdent); h > 0 {
			st, _ := procInfo(h)
			return h, st
		}
	}
	return 0, 0
}

const maxAncestorWalk = 10

func harnessPID(start int, parent func(int) (int, bool), ident func(int) string) int {
	pid := start
	for depth := 0; depth < maxAncestorWalk && pid > 1; depth++ {
		if identifiesHarness(ident(pid)) {
			return pid
		}
		p, ok := parent(pid)
		if !ok {
			break
		}
		pid = p
	}
	return 0
}

// Shell argv may mention ~/.claude; match executable basenames only.
func identifiesHarness(ident string) bool {
	sep := func(r rune) bool { return r == 0 || r == ' ' || r == '\t' || r == '\n' || r == '\r' }
	for _, tok := range strings.FieldsFunc(ident, sep) {
		if filepath.Base(tok) == "claude" {
			return true
		}
	}
	return false
}

func ownerFrom(s string) (int, int64) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n <= 0 {
		return 0, 0
	}
	st, _ := procInfo(n)
	return n, st
}
