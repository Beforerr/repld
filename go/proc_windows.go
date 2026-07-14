//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

const stillActive = 259

func sysProcAttrDetach() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: 0x00000008} // DETACHED_PROCESS
}

func terminateProc(p *os.Process) error {
	return p.Kill() // Windows has no SIGTERM; fall straight to a hard kill
}

// Windows cannot deliver console interrupt to detached children
// without console-attach gymnastics.
func interruptProc(_ *os.Process) error {
	return fmt.Errorf("interrupt signal not supported on windows")
}

func procInfo(pid int) (int64, bool) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		// Access restrictions must not make us reap a live owner.
		return 0, !errors.Is(err, windows.ERROR_INVALID_PARAMETER)
	}
	defer windows.CloseHandle(h)

	var exitCode uint32
	if err := windows.GetExitCodeProcess(h, &exitCode); err == nil && exitCode != stillActive {
		return 0, false
	}
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return 0, true
	}
	return created.Nanoseconds(), true
}

func ppidOf(_ int) (int, bool) { return 0, false }

func procIdent(_ int) string { return "" }
