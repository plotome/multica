package forkupdate

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// This models the release tools' idempotent disk transaction, separately from
// the orchestrator journal, so process restarts cannot erase observed state.
type diskDriver struct {
	root               string
	probe              Probe
	failInstall        bool
	failVerify         bool
	failRollback       bool
	probeErr           bool
	failRollbackVerify bool
	rollbackCalls      int
	installCalls       int
	afterInstall       func()
}

func (d *diskDriver) Probe(context.Context, Target) (Probe, error) {
	if d.probeErr {
		return Probe{}, errors.New("injected probe failure")
	}
	return d.probe, nil
}
func (d *diskDriver) Install(_ context.Context, target Target) error {
	d.installCalls++
	if d.failInstall {
		return errors.New("injected install failure")
	}
	if err := os.WriteFile(filepath.Join(d.root, target.Artifact.ID), []byte(target.Artifact.Commit), 0600); err != nil {
		return err
	}
	if d.afterInstall != nil {
		d.afterInstall()
	}
	return nil
}
func fixtureObservation(target Target, commit string) Observation {
	o := Observation{At: time.Now().UTC().Format(time.RFC3339Nano)}
	for _, kind := range ExpectedComponents(target.Artifact.Kind) {
		o.Components = append(o.Components, InstalledComponent{Kind: kind, Version: "fixture", Commit: commit})
	}
	return o
}
func (d *diskDriver) Verify(_ context.Context, target Target) (Observation, error) {
	if d.failVerify {
		return Observation{}, errors.New("running identity mismatch")
	}
	data, err := os.ReadFile(filepath.Join(d.root, target.Artifact.ID))
	if err != nil {
		return Observation{}, err
	}
	if string(data) != target.Artifact.Commit {
		return Observation{}, errors.New("wrong running commit")
	}
	return fixtureObservation(target, target.Artifact.Commit), nil
}
func (d *diskDriver) Rollback(_ context.Context, target Target) error {
	d.rollbackCalls++
	if d.failRollback {
		return errors.New("injected rollback interruption")
	}
	return os.WriteFile(filepath.Join(d.root, target.Artifact.ID), []byte(target.Baseline.Commit), 0600)
}
func (d *diskDriver) VerifyRollback(_ context.Context, target Target) (Observation, error) {
	if d.failRollbackVerify {
		return Observation{}, errors.New("injected rollback verification failure")
	}
	data, err := os.ReadFile(filepath.Join(d.root, target.Artifact.ID))
	if err != nil {
		return Observation{}, err
	}
	if string(data) != target.Baseline.Commit {
		return Observation{}, errors.New("wrong rollback identity")
	}
	return fixtureObservation(target, target.Baseline.Commit), nil
}

func journalFixture(t *testing.T) (Journal, *diskDriver) {
	t.Helper()
	c := verifiedFixture(t)
	r, err := Plan(c, inventoryFixture(c))
	if err != nil {
		t.Fatal(err)
	}
	j := Journal{Path: filepath.Join(t.TempDir(), "receipt.json"), LockDir: t.TempDir()}
	if err := j.Create(r); err != nil {
		t.Fatal(err)
	}
	return j, &diskDriver{root: t.TempDir(), probe: Probe{Online: true, RollbackAvailable: true}}
}

func approveFixture(t *testing.T, j Journal) {
	t.Helper()
	r, err := j.Read()
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Approve(r.Digest); err != nil {
		t.Fatal(err)
	}
}

func phaseFixture(t *testing.T, j Journal, id string) Phase {
	t.Helper()
	r, err := j.Read()
	if err != nil {
		t.Fatal(err)
	}
	return r.Targets[id].Phase
}

func TestRolloutApprovalBusyOfflineFailureResumeAndRollback(t *testing.T) {
	j, d := journalFixture(t)
	ctx := context.Background()
	if err := j.Advance(ctx, "mini-server", "mini", d); err == nil {
		t.Fatal("installed without approval")
	}
	if err := j.Approve("wrong-candidate"); err == nil {
		t.Fatal("approved mutable candidate")
	}
	approveFixture(t, j)
	if err := j.Advance(ctx, "air-desktop", "mini", d); err == nil {
		t.Fatal("installed on wrong host")
	}
	d.probe.Busy = true
	if err := j.Advance(ctx, "mini-server", "mini", d); err != nil {
		t.Fatal(err)
	}
	if d.installCalls != 0 || phaseFixture(t, j, "mini-server") != Waiting {
		t.Fatal("busy host was not deferred")
	}
	d.probe = Probe{}
	if err := j.Advance(ctx, "air-desktop", "air", d); err != nil {
		t.Fatal(err)
	}
	// A new Journal instance represents coordinator restart; approval survives.
	j = Journal{Path: j.Path, LockDir: j.LockDir}
	d.probe = Probe{Online: true, RollbackAvailable: true}
	for _, id := range []string{"mini-server", "mini-cli"} {
		if err := j.Advance(ctx, id, "mini", d); err != nil {
			t.Fatal(err)
		}
	}
	r, _ := j.Read()
	if r.Complete() {
		t.Fatal("partial success counted as complete")
	}
	d.failInstall = true
	if err := j.Advance(ctx, "air-desktop", "air", d); err == nil {
		t.Fatal("failure hidden")
	}
	if phaseFixture(t, j, "air-desktop") != Failed {
		t.Fatal("missing failure receipt")
	}
	d.failInstall = false
	if err := j.Retry("air-desktop"); err != nil {
		t.Fatal(err)
	}
	if err := j.Advance(ctx, "air-desktop", "air", d); err != nil {
		t.Fatal(err)
	}
	r, _ = j.Read()
	if !r.Complete() {
		t.Fatal("full verified rollout not complete")
	}
	calls := d.installCalls
	if err := j.Advance(ctx, "air-desktop", "air", d); err != nil || calls != d.installCalls {
		t.Fatal("duplicate installation")
	}
	if err := j.RequestRollback("air-desktop"); err != nil {
		t.Fatal(err)
	}
	d.failRollback = true
	if err := j.Advance(ctx, "air-desktop", "air", d); err == nil {
		t.Fatal("rollback failure hidden")
	}
	if phaseFixture(t, j, "air-desktop") != Failed {
		t.Fatal("lost rollback direction")
	}
	d.failRollback = false
	j = Journal{Path: j.Path, LockDir: j.LockDir}
	if err := j.Retry("air-desktop"); err != nil {
		t.Fatal(err)
	}
	if err := j.Advance(ctx, "air-desktop", "air", d); err != nil {
		t.Fatal(err)
	}
	if phaseFixture(t, j, "air-desktop") != RolledBack {
		t.Fatal("rollback not verified")
	}
	r, _ = j.Read()
	if r.Complete() {
		t.Fatal("rolled-back rollout called complete")
	}
}

func TestRecoverInterruptedInstallAndVerify(t *testing.T) {
	for _, checkpoint := range []Phase{Installing, Verifying} {
		t.Run(string(checkpoint), func(t *testing.T) {
			j, d := journalFixture(t)
			approveFixture(t, j)
			r, _ := j.Read()
			target := r.Targets["mini-server"]
			if err := d.Install(context.Background(), target); err != nil {
				t.Fatal(err)
			}
			target.Phase = checkpoint
			r.Targets["mini-server"] = target
			if err := j.withLock(func() error { return j.save(r) }); err != nil {
				t.Fatal(err)
			}
			j = Journal{Path: j.Path, LockDir: j.LockDir}
			d.installCalls = 0
			if err := j.Advance(context.Background(), "mini-server", "mini", d); err != nil {
				t.Fatal(err)
			}
			if checkpoint == Verifying && d.installCalls != 0 {
				t.Fatal("reinstalled during verification recovery")
			}
			if phaseFixture(t, j, "mini-server") != Succeeded {
				t.Fatal("recovery failed")
			}
		})
	}
}

func TestVerificationFailureAndConcurrentInstallAreExplicit(t *testing.T) {
	j, d := journalFixture(t)
	approveFixture(t, j)
	d.afterInstall = func() {
		// Separate Journal objects still share the OS lock.
		other := Journal{Path: j.Path, LockDir: j.LockDir}
		if err := other.Advance(context.Background(), "mini-server", "mini", d); err == nil {
			t.Error("concurrent installer acquired journal")
		}
	}
	d.failVerify = true
	if err := j.Advance(context.Background(), "mini-server", "mini", d); err == nil {
		t.Fatal("verification failure hidden")
	}
	r, _ := j.Read()
	if r.Complete() || r.Targets["mini-server"].Error != "running identity mismatch" {
		t.Fatal("wrong verification receipt")
	}
}

func TestUnknownStateAndCorruptReceiptCannotComplete(t *testing.T) {
	j, d := journalFixture(t)
	approveFixture(t, j)
	r, _ := j.Read()
	target := r.Targets["mini-server"]
	target.Phase = "future-state"
	r.Targets["mini-server"] = target
	if r.Complete() {
		t.Fatal("unknown state counted complete")
	}
	if err := j.withLock(func() error { return j.save(r) }); err != nil {
		t.Fatal(err)
	}
	if err := j.Advance(context.Background(), "mini-server", "mini", d); err == nil {
		t.Fatal("unknown state installed")
	}
	if err := os.WriteFile(j.Path, []byte(`{"schema":1`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Read(); err == nil {
		t.Fatal("corrupt receipt accepted")
	}
}

func TestProcessDeathReleasesLockAndResumesDiskTransaction(t *testing.T) {
	if path := os.Getenv("FORK_UPDATE_CRASH_RECEIPT"); path != "" {
		j := Journal{Path: path, LockDir: os.Getenv("FORK_UPDATE_HOST_LOCK_DIR")}
		d := &diskDriver{root: os.Getenv("FORK_UPDATE_CRASH_DISK"), probe: Probe{Online: true, RollbackAvailable: true}, afterInstall: func() { os.Exit(73) }}
		_ = j.Advance(context.Background(), "mini-server", "mini", d)
		os.Exit(74)
	}
	j, d := journalFixture(t)
	approveFixture(t, j)
	child := exec.Command(os.Args[0], "-test.run=^TestProcessDeathReleasesLockAndResumesDiskTransaction$")
	child.Env = append(os.Environ(), "FORK_UPDATE_CRASH_RECEIPT="+j.Path, "FORK_UPDATE_CRASH_DISK="+d.root, "FORK_UPDATE_HOST_LOCK_DIR="+j.LockDir)
	err := child.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 73 {
		t.Fatalf("child did not die after disk switch: %v", err)
	}
	if phaseFixture(t, j, "mini-server") != Installing {
		t.Fatal("install checkpoint was not durable before child died")
	}
	if _, err := d.Verify(context.Background(), Target{Artifact: catalogFixture().Artifacts[0]}); err != nil {
		t.Fatal("child never switched the fixture", err)
	}
	if err := (Journal{Path: j.Path, LockDir: j.LockDir}).Advance(context.Background(), "mini-server", "mini", d); err != nil {
		t.Fatal("could not reconcile after process death", err)
	}
	if phaseFixture(t, j, "mini-server") != Succeeded {
		t.Fatal("restarted worker did not verify")
	}
}

func TestRollbackProbeErrorAcrossProcessRestart(t *testing.T) {
	if path := os.Getenv("FORK_UPDATE_ROLLBACK_PROBE_RECEIPT"); path != "" {
		d := &diskDriver{probeErr: true}
		if err := (Journal{Path: path, LockDir: os.Getenv("FORK_UPDATE_HOST_LOCK_DIR")}).Advance(context.Background(), "mini-server", "mini", d); err == nil {
			t.Fatal("probe failure hidden")
		}
		return
	}
	j, d := journalFixture(t)
	approveFixture(t, j)
	if err := j.Advance(context.Background(), "mini-server", "mini", d); err != nil {
		t.Fatal(err)
	}
	if err := j.RequestRollback("mini-server"); err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestRollbackProbeErrorAcrossProcessRestart$")
	child.Env = append(os.Environ(), "FORK_UPDATE_ROLLBACK_PROBE_RECEIPT="+j.Path, "FORK_UPDATE_HOST_LOCK_DIR="+j.LockDir)
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("probe process: %s: %v", output, err)
	}
	j = Journal{Path: j.Path, LockDir: j.LockDir}
	r, err := j.Read()
	if err != nil {
		t.Fatal(err)
	}
	target := r.Targets["mini-server"]
	if target.Operation != "rollback" || target.FailureStage != "probe" || target.ResumePhase != RollingBack {
		t.Fatalf("lost failure direction: %#v", target)
	}
	if err := j.Retry("mini-server"); err != nil {
		t.Fatal(err)
	}
	before := d.installCalls
	if err := j.Advance(context.Background(), "mini-server", "mini", d); err != nil {
		t.Fatal(err)
	}
	if d.installCalls != before {
		t.Fatal("rollback recovery called Install and reinstalled candidate")
	}
	if phaseFixture(t, j, "mini-server") != RolledBack {
		t.Fatal("rollback direction was lost")
	}
}

func TestRollbackFailuresRequireExplicitDirectionPreservingRetry(t *testing.T) {
	for _, stage := range []string{"rollback", "rollback-verification"} {
		t.Run(stage, func(t *testing.T) {
			j, d := journalFixture(t)
			approveFixture(t, j)
			if err := j.Advance(context.Background(), "mini-server", "mini", d); err != nil {
				t.Fatal(err)
			}
			if err := j.RequestRollback("mini-server"); err != nil {
				t.Fatal(err)
			}
			d.failRollback = stage == "rollback"
			d.failRollbackVerify = stage == "rollback-verification"
			if err := j.Advance(context.Background(), "mini-server", "mini", d); err == nil {
				t.Fatal("failure hidden")
			}
			r, _ := j.Read()
			target := r.Targets["mini-server"]
			if target.Phase != Failed || target.Operation != "rollback" || target.FailureStage != stage {
				t.Fatalf("wrong failure: %#v", target)
			}
			calls := d.rollbackCalls
			d.failRollback = false
			d.failRollbackVerify = false
			if err := j.Advance(context.Background(), "mini-server", "mini", d); err != nil {
				t.Fatal(err)
			}
			if d.rollbackCalls != calls || phaseFixture(t, j, "mini-server") != Failed {
				t.Fatal("retried without explicit action")
			}
			if err := j.Retry("mini-server"); err != nil {
				t.Fatal(err)
			}
			if err := j.Advance(context.Background(), "mini-server", "mini", d); err != nil {
				t.Fatal(err)
			}
			if d.installCalls != 1 || phaseFixture(t, j, "mini-server") != RolledBack {
				t.Fatal("recovery installed candidate")
			}
			if stage == "rollback-verification" && d.rollbackCalls != calls {
				t.Fatal("verification retry repeated rollback")
			}
		})
	}
}

func TestRollbackRequiresAdapterAttestedBaseline(t *testing.T) {
	j, d := journalFixture(t)
	approveFixture(t, j)
	d.probe.RollbackAvailable = false
	if err := j.Advance(context.Background(), "mini-server", "mini", d); err != nil {
		t.Fatal(err)
	}
	if err := j.RequestRollback("mini-server"); err == nil {
		t.Fatal("unavailable baseline allowed rollback")
	}
}

func TestDifferentReceiptsCannotInstallSameHostConcurrently(t *testing.T) {
	first, d := journalFixture(t)
	approveFixture(t, first)
	second, _ := journalFixture(t)
	approveFixture(t, second)
	second.LockDir = first.LockDir
	d.afterInstall = func() {
		other := &diskDriver{root: t.TempDir(), probe: Probe{Online: true}}
		if err := second.Advance(context.Background(), "mini-cli", "mini", other); err == nil {
			t.Error("second transaction bypassed host installation lock")
		}
		if other.installCalls != 0 {
			t.Error("concurrent transaction installed")
		}
	}
	if err := first.Advance(context.Background(), "mini-server", "mini", d); err != nil {
		t.Fatal(err)
	}
	d.afterInstall = nil
	if err := second.Advance(context.Background(), "mini-cli", "mini", d); err != nil {
		t.Fatal("host lock not released", err)
	}
}
