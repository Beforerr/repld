//go:build windows

package main

import (
	"os"
	"syscall"
)

func sysProcAttrDetach() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: 0x00000008} // DETACHED_PROCESS
}

func terminateProc(p *os.Process) error {
	return p.Kill() // Windows has no SIGTERM; fall straight to a hard kill
}
