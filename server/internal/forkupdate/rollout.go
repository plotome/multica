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
	Prepared    Phase = "prepared"
	Waiting     Phase = "waiting"
	Installing  Phase = "installing"
	Verifying   Phase = "verifying"
	Succeeded   Phase = "succeeded"
	Failed      Phase = "failed"
	RollingBack Phase = "rolling-back"
	RolledBack  Phase = "rolled-back"
)

type Baseline struct {
	Host     string `json:"host"`
	Commit   string `json:"commit"`
	Sequence uint64 `json:"sequence"`
	Platform string `json:"platform"`
	Arch     string `json:"arch"`
}

type Target struct {
	Artifact Artifact `json:"artifact"`
	Baseline Baseline `json:"baseline"`
	Phase    Phase    `json:"phase"`
	Error    string   `json:"error,omitempty"`
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
		r.Targets[a.ID] = Target{Artifact: a, Baseline: b, Phase: Prepared}
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
	Online bool
	Busy   bool
}

// Driver implementations must use the existing host release tools. Install and
// Rollback must reconcile their transaction receipt on repeat calls. Verify
// must attest actual running identity, configuration and data preservation;
// success from a file rename or a spawned process is insufficient.
type Driver interface {
	Probe(context.Context, Target) (Probe, error)
	Install(context.Context, Target) error
	Verify(context.Context, Target) error
	Rollback(context.Context, Target) error
	VerifyRollback(context.Context, Target) error
}

type Journal struct{ Path string }

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
	return syncDirectory(filepath.Dir(j.Path))
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
		case Prepared, Waiting, Installing, Verifying, RollingBack:
		default:
			return fmt.Errorf("unknown target state %q", t.Phase)
		}
		persist := func(phase Phase, message string) error {
			t.Phase, t.Error = phase, message
			r.Targets[id] = t
			return j.save(r)
		}
		fail := func(err error) error {
			if saveErr := persist(Failed, err.Error()); saveErr != nil {
				return errors.Join(err, saveErr)
			}
			return err
		}
		probe, err := driver.Probe(ctx, t)
		if err != nil {
			return fail(err)
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
		if t.Phase == RollingBack {
			if err := driver.Rollback(ctx, t); err != nil {
				// Retain rollback direction on failure so resume cannot install.
				if saveErr := persist(RollingBack, err.Error()); saveErr != nil {
					return errors.Join(err, saveErr)
				}
				return err
			}
			if err := driver.VerifyRollback(ctx, t); err != nil {
				if saveErr := persist(RollingBack, err.Error()); saveErr != nil {
					return errors.Join(err, saveErr)
				}
				return err
			}
			return persist(RolledBack, "")
		}
		if t.Phase != Verifying {
			if err := persist(Installing, ""); err != nil {
				return err
			}
			if err := driver.Install(ctx, t); err != nil {
				return fail(err)
			}
			if err := persist(Verifying, ""); err != nil {
				return err
			}
		}
		if err := driver.Verify(ctx, t); err != nil {
			return fail(err)
		}
		return persist(Succeeded, "")
	})
}

func (j Journal) Retry(id string) error {
	return j.withLock(func() error {
		r, err := j.load()
		if err != nil {
			return err
		}
		t, ok := r.Targets[id]
		if !ok || t.Phase != Failed {
			return errors.New("only a failed target can be retried")
		}
		t.Phase, t.Error = Installing, ""
		r.Targets[id] = t
		return j.save(r)
	})
}

func (j Journal) RequestRollback(id string) error {
	return j.withLock(func() error {
		r, err := j.load()
		if err != nil {
			return err
		}
		t, ok := r.Targets[id]
		if !ok || (t.Phase != Failed && t.Phase != Succeeded && t.Phase != Installing && t.Phase != Verifying) {
			return errors.New("target has no installation to roll back")
		}
		t.Phase, t.Error = RollingBack, ""
		r.Targets[id] = t
		return j.save(r)
	})
}
