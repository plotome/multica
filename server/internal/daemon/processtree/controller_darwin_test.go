//go:build darwin

package processtree

import (
	"errors"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestFinishRecognizesZombieOnlyGroup(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	c, err := newController(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Wait()
	deadline := time.Now().Add(processTreeFinishTimeout)
	for {
		rows, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", cmd.Process.Pid)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 1 && rows[0].Proc.P_stat == 5 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child did not become a waitable zombie")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := syscall.Kill(-cmd.Process.Pid, 0); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("zombie-only Darwin group probe = %v, want EPERM", err)
	}
	if err := c.finish(cmd); err != nil {
		t.Fatalf("finished zombie-only group: %v", err)
	}
}

func TestLiveGroupPermissionErrorIsNotCompletion(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "sleep 30")
	if _, err := newController(cmd); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); _ = cmd.Wait() }()
	if groupHasFinished(cmd.Process.Pid, syscall.EPERM) {
		t.Fatal("a live group must not be mistaken for a zombie-only group")
	}
	if groupHasFinished(cmd.Process.Pid, nil) {
		t.Fatal("successful probe must not imply completion")
	}
}

func TestFinishedGroupRecognizesStalePermissionError(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if _, err := newController(cmd); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(-cmd.Process.Pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("reaped group probe = %v, want ESRCH", err)
	}
	if !groupHasNoLiveMembers(cmd.Process.Pid) {
		t.Fatal("an empty Darwin process snapshot must count as having no live members")
	}
	if !groupHasFinished(cmd.Process.Pid, syscall.EPERM) {
		t.Fatal("a group gone after an earlier EPERM must be recognized as finished")
	}
}

// A signal can race kernel teardown even while the next membership check
// still sees a live/exiting member. Completion must be verified afterwards.
func TestFinishVerifiesExitAfterPermissionRace(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "sleep 30")
	if _, err := newController(cmd); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); _ = cmd.Wait() }()
	calls := 0
	err := finishGroup(cmd.Process.Pid, func(pid int, sig syscall.Signal) error {
		calls++
		if pid != -cmd.Process.Pid {
			t.Fatalf("unexpected group %d", pid)
		}
		switch calls {
		case 1:
			if sig != 0 {
				t.Fatalf("first probe signal %v", sig)
			}
			return nil
		case 2:
			if sig != syscall.SIGKILL {
				t.Fatalf("cleanup signal %v", sig)
			}
			return syscall.EPERM
		default:
			if sig != 0 {
				t.Fatalf("exit probe signal %v", sig)
			}
			return syscall.ESRCH
		}
	})
	if err != nil {
		t.Fatalf("verified exit after transient signal error: %v", err)
	}
	if calls != 3 {
		t.Fatalf("got %d signal calls, want initial probe, cleanup, exit proof", calls)
	}
}

func TestFinishPreservesLiveGroupPermissionError(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "sleep 30")
	if _, err := newController(cmd); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); _ = cmd.Wait() }()
	err := finishGroup(cmd.Process.Pid, func(int, syscall.Signal) error { return syscall.EPERM })
	if !errors.Is(err, syscall.EPERM) {
		t.Fatalf("live denied group error = %v, want EPERM", err)
	}
}
