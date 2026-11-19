package forkupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type Phase string

const (
	Prepared          Phase = "prepared"
	Downloading       Phase = "downloading"
	Validating        Phase = "validating"
	Waiting           Phase = "waiting"
	Installing        Phase = "installing"
	Verifying         Phase = "verifying"
	Succeeded         Phase = "succeeded"
	Failed            Phase = "failed"
	RollingBack       Phase = "rolling-back"
	VerifyingRollback Phase = "verifying-rollback"
	RolledBack        Phase = "rolled-back"
)

type Baseline struct {
	Host     string `json:"host"`
	Commit   string `json:"commit"`
	Sequence uint64 `json:"sequence"`
	Platform string `json:"platform"`
	Arch     string `json:"arch"`
}

type Target struct {
	Artifact          Artifact     `json:"artifact"`
	Baseline          Baseline     `json:"baseline"`
	Phase             Phase        `json:"phase"`
	Error             string       `json:"error,omitempty"`
	Log               string       `json:"log,omitempty"`
	Operation         string       `json:"operation"`
	FailureStage      string       `json:"failure_stage,omitempty"`
	ResumePhase       Phase        `json:"resume_phase,omitempty"`
	RollbackAvailable bool         `json:"rollback_available"`
	PackageReady      bool         `json:"package_ready"`
	Attempt           uint64       `json:"attempt"`
	Observation       *Observation `json:"observation,omitempty"`
}

// Receipt is a local technical operation journal, not a replacement for the
// server's workspace records. It must be kept outside the daemon/app lifecycle.
type Receipt struct {
	Schema   int               `json:"schema"`
	Digest   string            `json:"digest"`
	Sequence uint64            `json:"sequence"`
	Approved bool              `json:"approved"`
	Targets  map[string]Target `json:"targets"`
}

// Plan requires the full installed inventory. Missing components cannot vanish
// from the denominator and make a partial rollout look complete.
func Plan(candidate VerifiedCatalog, installed map[string]Baseline) (Receipt, error) {
	r := Receipt{Schema: 1, Digest: candidate.Digest, Sequence: candidate.Sequence, Targets: map[string]Target{}}
	if !digestPattern.MatchString(candidate.Digest) || len(installed) == 0 || len(candidate.Artifacts) != len(installed) {
		return r, errors.New("candidate and installed inventory must match exactly")
	}
	for _, a := range candidate.Artifacts {
		b, ok := installed[a.ID]
		if !ok || !commitPattern.MatchString(b.Commit) || candidate.Sequence <= b.Sequence || a.Host != b.Host || a.Platform != b.Platform || a.Arch != b.Arch {
			return r, fmt.Errorf("artifact %q: missing inventory, downgrade or wrong platform", a.ID)
		}
		if _, duplicate := r.Targets[a.ID]; duplicate {
			return r, fmt.Errorf("duplicate target %q", a.ID)
		}
		r.Targets[a.ID] = Target{Artifact: a, Baseline: b, Phase: Prepared, Operation: "install", Attempt: 1}
	}
	return r, nil
}

func (r Receipt) Complete() bool {
	if r.Schema != 1 || r.Sequence == 0 || !digestPattern.MatchString(r.Digest) || !r.Approved || len(r.Targets) == 0 {
		return false
	}
	for _, t := range r.Targets {
		if t.Phase != Succeeded {
			return false
		}
	}
	return true
}

type Probe struct {
	Online            bool
	Busy              bool
	RollbackAvailable bool
	Observation       *Observation
}

// Driver implementations must use the existing host release tools. Install and
// Rollback must reconcile their transaction receipt on repeat calls. Verify
// must attest actual running identity, configuration and data preservation;
// success from a file rename or a spawned process is insufficient.
type Driver interface {
	Probe(context.Context, Target) (Probe, error)
	Install(context.Context, Target) error
	Verify(context.Context, Target) (Observation, error)
	Rollback(context.Context, Target) error
	VerifyRollback(context.Context, Target) (Observation, error)
}

type Journal struct {
	Path string
	// LockDir is a host-local policy path shared by all update generations.
	// It must not be derived from a transaction ID or supplied by a catalog.
	LockDir      string
	OnCheckpoint func(Receipt)
}

func (j Journal) load() (Receipt, error) {
	var r Receipt
	data, err := os.ReadFile(j.Path)
	if err == nil {
		err = decodeStrict(data, &r)
	}
	if err == nil && (r.Schema != 1 || r.Sequence == 0 || !digestPattern.MatchString(r.Digest) || len(r.Targets) == 0) {
		err = errors.New("invalid update receipt")
	}
	return r, err
}

func (j Journal) save(r Receipt) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(j.Path), ".fork-update-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(name, j.Path); err != nil {
		return err
	}
	if err := syncDirectory(filepath.Dir(j.Path)); err != nil {
		return err
	}
	if j.OnCheckpoint != nil {
		j.OnCheckpoint(r)
	}
	return nil
}

func (j Journal) withLock(fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(j.Path), 0700); err != nil {
		return err
	}
	unlock, err := lockJournal(j.Path + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	return fn()
}

func (j Journal) Create(r Receipt) error {
	return j.withLock(func() error {
		if _, err := os.Lstat(j.Path); !os.IsNotExist(err) {
			return errors.New("update receipt already exists or cannot be inspected")
		}
		if r.Schema != 1 || r.Sequence == 0 || r.Approved || !digestPattern.MatchString(r.Digest) || len(r.Targets) == 0 {
			return errors.New("invalid unapproved plan")
		}
		for id, target := range r.Targets {
			if target.Artifact.ID != id || target.Phase != Prepared {
				return errors.New("new receipt must contain only prepared targets")
			}
		}
		return j.save(r)
	})
}

func (j Journal) Read() (Receipt, error) {
	var r Receipt
	err := j.withLock(func() error {
		var err error
		r, err = j.load()
		return err
	})
	return r, err
}

// Approve pins one reviewed candidate; approval never follows a moving latest.
func (j Journal) Approve(digest string) error {
	return j.withLock(func() error {
		r, err := j.load()
		if err != nil {
			return err
		}
		if digest != r.Digest {
			return errors.New("approval is for a different candidate")
		}
		r.Approved = true
		return j.save(r)
	})
}

// Advance runs one host-local target. An external supervised worker calls it;
// the daemon must never own the process that will restart it. A failed target
// requires Retry; disconnection and busy state retain approval indefinitely.
func (j Journal) Advance(ctx context.Context, id, host string, driver Driver) error {
	if !filepath.IsAbs(j.LockDir) || !idPattern.MatchString(host) {
		return errors.New("a shared absolute host installation lock directory is required")
	}
	if err := os.MkdirAll(j.LockDir, 0700); err != nil {
		return err
	}
	unlock, err := lockJournal(filepath.Join(j.LockDir, host+".lock"))
	if err != nil {
		return err
	}
	defer unlock()
	return j.withLock(func() error {
		r, err := j.load()
		if err != nil {
			return err
		}
		t, ok := r.Targets[id]
		if !ok || t.Artifact.Host != host {
			return errors.New("target is not assigned to this host")
		}
		if !r.Approved {
			return errors.New("explicit candidate approval required")
		}
		switch t.Phase {
		case Succeeded, Failed, RolledBack:
			return nil
		case Prepared, Downloading, Validating, Waiting, Installing, Verifying, RollingBack, VerifyingRollback:
		default:
			return fmt.Errorf("unknown target state %q", t.Phase)
		}
		persist := func(phase Phase, message string) error {
			t.Phase, t.Error = phase, message
			r.Targets[id] = t
			return j.save(r)
		}
		fail := func(stage string, err error) error {
			t.FailureStage, t.ResumePhase = stage, t.Phase
			t.Log = err.Error()
			if saveErr := persist(Failed, err.Error()); saveErr != nil {
				return errors.Join(err, saveErr)
			}
			return err
		}
		probe, err := driver.Probe(ctx, t)
		if err != nil {
			return fail("probe", err)
		}
		t.RollbackAvailable = probe.RollbackAvailable
		if probe.Observation != nil {
			t.Observation = probe.Observation
		}
		if !probe.Online || probe.Busy {
			// Preserve interrupted install/verify/rollback checkpoints. Waiting
			// is a pre-install state, never a reason to rerun a completed switch.
			if t.Phase == Prepared || t.Phase == Waiting {
				reason := "waiting for host"
				if probe.Online {
					reason = "waiting for idle"
				}
				return persist(Waiting, reason)
			}
			return nil
		}
		if t.Operation == "rollback" {
			if t.Phase != RollingBack && t.Phase != VerifyingRollback {
				return errors.New("rollback operation has an invalid checkpoint")
			}
			if t.Phase == RollingBack {
				if !probe.RollbackAvailable {
					return fail("rollback", errors.New("recoverable baseline is unavailable"))
				}
				if err := driver.Rollback(ctx, t); err != nil {
					return fail("rollback", err)
				}
				if err := persist(VerifyingRollback, ""); err != nil {
					return err
				}
			}
			observation, err := driver.VerifyRollback(ctx, t)
			if err == nil {
				err = observation.Validate(t.Artifact.Kind, t.Baseline.Commit)
			}
			if err != nil {
				return fail("rollback-verification", err)
			}
			t.Observation = &observation
			return persist(RolledBack, "")
		}
		if t.Operation != "install" {
			return errors.New("unknown update operation")
		}
		if t.Phase != Verifying {
			if err := persist(Installing, ""); err != nil {
				return err
			}
			if err := driver.Install(ctx, t); err != nil {
				return fail("install", err)
			}
			if err := persist(Verifying, ""); err != nil {
				return err
			}
		}
		observation, err := driver.Verify(ctx, t)
		if err == nil {
			err = observation.Validate(t.Artifact.Kind, t.Artifact.Commit)
		}
		if err != nil {
			return fail("running-verification", err)
		}
		t.Observation = &observation
		return persist(Succeeded, "")
	})
}

func (j Journal) Retry(id string) error {
	return j.withLock(func() error {
		r, err := j.load()
		if err != nil {
			return err
		}
		if err := RetryTarget(&r, id); err != nil {
			return err
		}
		return j.save(r)
	})
}

func RetryTarget(r *Receipt, id string) error {
	t, ok := r.Targets[id]
	if !ok || t.Phase != Failed {
		return errors.New("only a failed target can be retried")
	}
	switch t.Operation {
	case "rollback":
		if t.ResumePhase != RollingBack && t.ResumePhase != VerifyingRollback {
			return errors.New("invalid rollback recovery checkpoint")
		}
	case "install":
		if t.ResumePhase != Prepared && t.ResumePhase != Waiting && t.ResumePhase != Installing && t.ResumePhase != Verifying {
			return errors.New("invalid install recovery checkpoint")
		}
	default:
		return errors.New("unknown recovery operation")
	}
	t.Phase, t.Error = t.ResumePhase, ""
	t.ResumePhase, t.FailureStage = "", ""
	t.Attempt++
	r.Targets[id] = t
	return nil
}

func (j Journal) RequestRollback(id string) error {
	return j.withLock(func() error {
		r, err := j.load()
		if err != nil {
			return err
		}
		if err := RequestTargetRollback(&r, id); err != nil {
			return err
		}
		return j.save(r)
	})
}

func RequestTargetRollback(r *Receipt, id string) error {
	t, ok := r.Targets[id]
	if !ok || t.Operation != "install" || !t.RollbackAvailable || (t.Phase != Failed && t.Phase != Succeeded && t.Phase != Installing && t.Phase != Verifying) {
		return errors.New("target has no installation to roll back")
	}
	t.Phase, t.Error = RollingBack, ""
	t.Operation = "rollback"
	t.Attempt++
	t.ResumePhase, t.FailureStage = "", ""
	r.Targets[id] = t
	return nil
}
