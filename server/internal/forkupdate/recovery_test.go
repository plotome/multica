package forkupdate

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
)

func TestUnknownDatabaseCompatibilityStopsBeforeInstallation(t *testing.T) {
	j, d := journalFixture(t)
	approveFixture(t, j)
	d.recoveryUnknown = true
	if err := j.Advance(context.Background(), "mini-server", "mini", d); err != nil {
		t.Fatal(err)
	}
	r, _ := j.Read()
	target := r.Targets["mini-server"]
	if target.Phase != Prepared || target.Error != "database compatibility unknown" || d.installCalls != 0 {
		t.Fatal("unverified compatibility crossed installation boundary")
	}
}
func TestCandidateFailureRestoresBaselineAndRemainsFailed(t *testing.T) {
	j, d := journalFixture(t)
	approveFixture(t, j)
	d.failVerify = true
	if err := j.Advance(context.Background(), "mini-server", "mini", d); err == nil {
		t.Fatal("candidate failure hidden")
	}
	r, _ := j.Read()
	target := r.Targets["mini-server"]
	if target.Phase != Failed || target.Recovery.Status != "restored" || target.Observation.Components[0].Commit != target.Baseline.Commit || r.Complete() {
		t.Fatal("restored baseline counted as candidate success")
	}
	calls := d.installCalls
	if err := j.Advance(context.Background(), "mini-server", "mini", d); err != nil || d.installCalls != calls {
		t.Fatal("failed candidate reinstalled without approval retry")
	}
	if err := j.Retry("mini-server"); err != nil {
		t.Fatal(err)
	}
	d.failVerify = false
	if err := j.Advance(context.Background(), "mini-server", "mini", d); err != nil {
		t.Fatal(err)
	}
	if d.installCalls != calls+1 || phaseFixture(t, j, "mini-server") != Succeeded {
		t.Fatal("explicit candidate retry did not install and verify")
	}
}
func TestRestorationFailureRequiresExplicitRestorationRetry(t *testing.T) {
	for _, verify := range []bool{false, true} {
		t.Run(map[bool]string{false: "execute", true: "verify"}[verify], func(t *testing.T) {
			j, d := journalFixture(t)
			approveFixture(t, j)
			d.failVerify = true
			d.failRollback = !verify
			d.failRollbackVerify = verify
			if err := j.Advance(context.Background(), "mini-server", "mini", d); err == nil {
				t.Fatal("restoration failure hidden")
			}
			r, _ := j.Read()
			target := r.Targets["mini-server"]
			if target.Recovery.Status != "failed" || target.Recovery.CandidateError == "" || target.Recovery.Error == "" {
				t.Fatal("failure evidence missing")
			}
			installs, restores := d.installCalls, d.rollbackCalls
			d.failRollback = false
			d.failRollbackVerify = false
			if err := j.Advance(context.Background(), "mini-server", "mini", d); err != nil || d.rollbackCalls != restores {
				t.Fatal("restoration retried without explicit retry")
			}
			if err := j.Retry("mini-server"); err != nil {
				t.Fatal(err)
			}
			j = Journal{Path: j.Path, LockDir: j.LockDir}
			_ = j.Advance(context.Background(), "mini-server", "mini", d)
			r, _ = j.Read()
			if r.Targets["mini-server"].Recovery.Status != "restored" || d.installCalls != installs {
				t.Fatal("recovery retry reinstalled candidate")
			}
		})
	}
}
func TestRestorationResumesAfterUpdaterProcessDeath(t *testing.T) {
	if path := os.Getenv("FORK_RECOVERY_CRASH_RECEIPT"); path != "" {
		d := &diskDriver{root: os.Getenv("FORK_RECOVERY_CRASH_DISK"), probe: Probe{Online: true}, failVerify: true, afterRollback: func() { os.Exit(75) }}
		_ = (Journal{Path: path, LockDir: os.Getenv("FORK_RECOVERY_CRASH_LOCKS")}).Advance(context.Background(), "mini-server", "mini", d)
		os.Exit(76)
	}
	j, d := journalFixture(t)
	approveFixture(t, j)
	child := exec.Command(os.Args[0], "-test.run=^TestRestorationResumesAfterUpdaterProcessDeath$")
	child.Env = append(os.Environ(), "FORK_RECOVERY_CRASH_RECEIPT="+j.Path, "FORK_RECOVERY_CRASH_DISK="+d.root, "FORK_RECOVERY_CRASH_LOCKS="+j.LockDir)
	err := child.Run()
	var code *exec.ExitError
	if !errors.As(err, &code) || code.ExitCode() != 75 {
		t.Fatalf("did not interrupt real restoration process: %v", err)
	}
	r, _ := j.Read()
	if r.Targets["mini-server"].Phase != Restoring {
		t.Fatal("direction not durable before restoration")
	}
	_ = (Journal{Path: j.Path, LockDir: j.LockDir}).Advance(context.Background(), "mini-server", "mini", d)
	r, _ = j.Read()
	if d.installCalls != 0 || r.Targets["mini-server"].Recovery.Status != "restored" {
		t.Fatal("restart reinstalled candidate or lost recovery")
	}
}
func TestControlOutageDuringAutomaticRestorationAndResultRedelivery(t *testing.T) {
	c, control, d := fixtureConsumer(t)
	control.state.Receipt.Approved = true
	d.failVerify = true
	d.afterInstall = func() { control.mu.Lock(); control.offline = true; control.mu.Unlock() }
	if err := c.Tick(context.Background()); err == nil {
		t.Fatal("candidate failure and control outage hidden")
	}
	control.mu.Lock()
	control.offline = false
	control.mu.Unlock()
	if err := c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	target := control.state.Receipt.Targets["mini-server"]
	if target.Phase != Failed || target.Recovery.Status != "restored" || d.installCalls != 1 {
		t.Fatal("restored result was not redelivered")
	}
	if err := control.state.Action("retry", control.state.Receipt.Digest, "mini-server"); err != nil {
		t.Fatal(err)
	}
	control.state.Revision++
	d.failVerify = false
	d.afterInstall = nil
	if err := c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !control.state.Receipt.Complete() || d.installCalls != 2 {
		t.Fatal("remote retry failed after baseline restoration")
	}
}
