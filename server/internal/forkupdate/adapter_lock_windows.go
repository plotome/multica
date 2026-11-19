package forkupdate

import (
	"context"
	"errors"
	"os/exec"
)

func inheritHostLock(ctx context.Context, cmd *exec.Cmd) error {
	if ctx.Value(hostLockContextKey{}) != nil {
		return errors.New("native coordinated release adapters require a Unix host")
	}
	return nil
}
func DetachInstallerLockOnDaemonExec() {}
