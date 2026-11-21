//go:build darwin

package processtree

import "golang.org/x/sys/unix"

// Darwin can retain a process group after its last member disappears, so
// kill(-pgid, 0) still reports EPERM while the process snapshot is empty.
// Both an empty snapshot and a zombie-only snapshot prove no live member
// remains; a failed snapshot never does.
func groupHasNoLiveMembers(pid int) bool {
	rows, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", pid)
	if err != nil {
		return false
	}
	for _, row := range rows {
		// SZOMB in Darwin sys/proc.h: exited, awaiting collection by parent.
		if row.Proc.P_stat != 5 {
			return false
		}
	}
	return true
}
