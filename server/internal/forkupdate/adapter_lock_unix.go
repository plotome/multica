//go:build !windows

package forkupdate

import (
	"context"
	"golang.org/x/sys/unix"
	"os"
	"os/exec"
	"strconv"
)

func inheritHostLock(ctx context.Context, cmd *exec.Cmd) error {
	if file, ok := ctx.Value(hostLockContextKey{}).(*os.File); ok {
		cmd.ExtraFiles = append(cmd.ExtraFiles, file)
		cmd.Env = append(os.Environ(), "MULTICA_UPDATE_HOST_LOCK_FD="+strconv.Itoa(2+len(cmd.ExtraFiles)))
	}
	return nil
}

// Background daemons must not keep the installer's open-file-description lock.
// The foreground lifecycle CLI retains it until it exits; exec closes it only
// in the new long-lived daemon process.
func DetachInstallerLockOnDaemonExec() {
	if value := os.Getenv("MULTICA_UPDATE_HOST_LOCK_FD"); value != "" {
		if fd, err := strconv.Atoi(value); err == nil && fd >= 3 {
			unix.CloseOnExec(fd)
		}
	}
}
