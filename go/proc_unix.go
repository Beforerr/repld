//go:build !windows

package main

import (
	"os"
	"syscall"
)

func sysProcAttrDetach() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

func terminateProc(p *os.Process) error {
	return p.Signal(syscall.SIGTERM)
}
