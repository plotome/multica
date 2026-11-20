//go:build darwin

package processtree

import "golang.org/x/sys/unix"

func zombieOnlyGroup(pid int) bool {
	rows, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", pid)
	if err != nil || len(rows) == 0 {
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
