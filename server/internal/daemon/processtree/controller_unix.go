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

func (c *controller) interrupt(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return os.ErrProcessDone
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
		if groupHasFinished(cmd.Process.Pid, err) {
			return nil
		}
		if errors.Is(err, syscall.EPERM) {
			// A permission result may race group teardown. The shared finish
			// path proves completion within its existing deadline.
			return c.finish(cmd)
		}
		return err
	}
	return nil
}

func (c *controller) stop(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return os.ErrProcessDone
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		if groupHasFinished(cmd.Process.Pid, err) {
			return nil
		}
		if errors.Is(err, syscall.EPERM) {
			// A permission result may race group teardown. The shared finish
			// path proves completion within its existing deadline.
			return c.finish(cmd)
		}
		return cmd.Process.Kill()
	}
	return nil
}

func (c *controller) finish(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return finishGroup(cmd.Process.Pid, syscall.Kill)
}

// finishGroup keeps signal delivery and subsequent liveness checks separate.
func finishGroup(pid int, signal func(int, syscall.Signal) error) error {
	if err := signal(-pid, 0); groupHasFinished(pid, err) {
		return nil
	}
	// A normally-exited leader can still leave a descendant holding inherited
	// pipes or repository locks. Kill the remaining group before returning
	// ownership of the repository to another operation.
	signalErr := signal(-pid, syscall.SIGKILL)
	if groupHasFinished(pid, signalErr) {
		return nil
	}
	if signalErr != nil && !errors.Is(signalErr, syscall.EPERM) {
		return signalErr
	}
	// Darwin's signal and process snapshots can observe different teardown
	// phases. EPERM is neither success nor a final failure here: use the
	// existing finish deadline to prove exit, retaining the error if the
	// group stays live. Never return repository ownership on EPERM alone.
	deadline := time.Now().Add(processTreeFinishTimeout)
	var lastErr error
	for time.Now().Before(deadline) {
		lastErr = signal(-pid, 0)
		if groupHasFinished(pid, lastErr) {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := errors.Join(signalErr, lastErr); err != nil {
		return fmt.Errorf("process group %d still active after %s: %w", pid, processTreeFinishTimeout, err)
	}
	return fmt.Errorf("process group %d still active after %s", pid, processTreeFinishTimeout)
}

// EPERM alone is not evidence of completion: a live group may be owned by
// someone else. Darwin also returns it for groups with no live members.
func groupHasFinished(pid int, err error) bool {
	if errors.Is(err, syscall.ESRCH) {
		return true
	}
	if !errors.Is(err, syscall.EPERM) {
		return false
	}
	if groupHasNoLiveMembers(pid) {
		return true
	}
	// The group can disappear between the failed signal and the process
	// snapshot. Only a fresh ESRCH proves that transition; EPERM stays live.
	return errors.Is(syscall.Kill(-pid, 0), syscall.ESRCH)
}

func (*controller) close() {}
