//go:build darwin

package discovery

import (
	"strconv"

	"golang.org/x/sys/unix"
)

func darwinProcessStartIdentity(pid int) string {
	if pid <= 0 {
		return ""
	}
	process, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || int(process.Proc.P_pid) != pid {
		return ""
	}
	started := process.Proc.P_starttime
	if started.Sec <= 0 || started.Usec < 0 || started.Usec >= 1_000_000 {
		return ""
	}
	return "darwin:" + strconv.FormatInt(started.Sec, 10) + ":" + strconv.FormatInt(int64(started.Usec), 10)
}
