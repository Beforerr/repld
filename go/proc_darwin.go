package main

import "golang.org/x/sys/unix"

func procInfo(pid int) (startTime int64, alive bool) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || kp == nil {
		return 0, false
	}
	tv := kp.Proc.P_starttime
	return int64(tv.Sec)*1e9 + int64(tv.Usec)*1e3, true
}

func ppidOf(pid int) (int, bool) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || kp == nil {
		return 0, false
	}
	return int(kp.Eproc.Ppid), true
}

// procIdent returns the process's comm plus its raw argv buffer (KERN_PROCARGS2,
// readable only for same-user processes). Empty when nothing is readable.
func procIdent(pid int) string {
	var b []byte
	if kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid); err == nil && kp != nil {
		c := kp.Proc.P_comm
		for _, ch := range c {
			if ch == 0 {
				break
			}
			b = append(b, byte(ch))
		}
		b = append(b, 0)
	}
	if raw, err := unix.SysctlRaw("kern.procargs2", pid); err == nil {
		b = append(b, raw...)
	}
	return string(b)
}
