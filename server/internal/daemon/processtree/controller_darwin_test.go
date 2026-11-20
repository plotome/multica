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
