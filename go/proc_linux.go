package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// statFields returns the space-split fields of /proc/<pid>/stat after the comm
// field. comm is parenthesized and may contain spaces, so split after the last
// ')': fields[0] is the state (field 3), so 1-indexed field N is fields[N-3].
func statFields(pid int) ([]string, bool) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return nil, false
	}
	i := strings.LastIndexByte(string(b), ')')
	if i < 0 {
		return nil, false
	}
	return strings.Fields(string(b)[i+1:]), true
}

func procInfo(pid int) (startTime int64, alive bool) {
	f, ok := statFields(pid)
	if !ok || len(f) < 20 {
		return 0, false
	}
	st, _ := strconv.ParseInt(f[19], 10, 64) // field 22: starttime in clock ticks
	return st, true
}

func ppidOf(pid int) (int, bool) {
	f, ok := statFields(pid)
	if !ok || len(f) < 2 {
		return 0, false
	}
	ppid, _ := strconv.Atoi(f[1]) // field 4
	return ppid, true
}

func procIdent(pid int) string {
	comm, _ := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	cmd, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)) // NUL-separated argv
	return string(comm) + "\x00" + string(cmd)
}
