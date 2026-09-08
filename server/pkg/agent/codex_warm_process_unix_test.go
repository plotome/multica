//go:build !windows

package agent

import (
	"errors"
	"syscall"
)

func codexWarmTestProcessGone(pid int) bool {
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}
