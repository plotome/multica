//go:build !windows

package processtree

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

const processTreeFinishTimeout = 5 * time.Second

type controller struct{}

func newController(cmd *exec.Cmd) (*controller, error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	return &controller{}, nil
}

func (*controller) attach(_ *exec.Cmd) error { return nil }

func (*controller) interrupt(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return os.ErrProcessDone
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
		if groupHasFinished(cmd.Process.Pid, err) {
			return nil
		}
		return err
	}
	return nil
}

func (*controller) stop(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return os.ErrProcessDone
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		if groupHasFinished(cmd.Process.Pid, err) {
			return nil
		}
		return cmd.Process.Kill()
	}
	return nil
}

func (c *controller) finish(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	pid := cmd.Process.Pid
	if err := syscall.Kill(-pid, 0); groupHasFinished(cmd.Process.Pid, err) {
		return nil
	}
	// A normally-exited leader can still leave a descendant holding inherited
	// pipes or repository locks. Kill the remaining group before returning
	// ownership of the repository to another operation.
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !groupHasFinished(cmd.Process.Pid, err) {
		return err
	}
	deadline := time.Now().Add(processTreeFinishTimeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(-pid, 0); groupHasFinished(cmd.Process.Pid, err) {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("process group %d still active after %s", pid, processTreeFinishTimeout)
}

// EPERM alone is not evidence of completion: a live group may be owned by
// someone else. Darwin also returns it for groups containing only zombies.
func groupHasFinished(pid int, err error) bool {
	return errors.Is(err, syscall.ESRCH) ||
		(errors.Is(err, syscall.EPERM) && zombieOnlyGroup(pid))
}

func (*controller) close() {}
