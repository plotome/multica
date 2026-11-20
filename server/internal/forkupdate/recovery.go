package forkupdate

import (
	"context"
	"errors"
)

const (
	Restoring        Phase = "restoring-baseline"
	VerifyingRestore Phase = "verifying-baseline"
)

// RecoveryPlan is produced by the local release adapter after checking the
// running baseline, immutable recovery assets and both migration directions.
// It is saved in the approved receipt before any service can be paused.
type RecoveryPlan struct {
	Receipt             string      `json:"receipt,omitempty"`
	CandidateCommit     string      `json:"candidate_commit"`
	BaselineCommit      string      `json:"baseline_commit"`
	AssetsSHA256        string      `json:"assets_sha256"`
	CompatibilitySHA256 string      `json:"compatibility_sha256"`
	ForwardCompatible   bool        `json:"forward_compatible"`
	BackwardCompatible  bool        `json:"backward_compatible"`
	Baseline            Observation `json:"baseline"`
}
type Recovery struct {
	Plan           RecoveryPlan `json:"plan"`
	Status         string       `json:"status"`
	CandidateError string       `json:"candidate_error,omitempty"`
	CandidateStage string       `json:"candidate_stage,omitempty"`
	Error          string       `json:"error,omitempty"`
}
type RecoveryPlanner interface {
	PrepareRecovery(context.Context, Target) (RecoveryPlan, error)
}

func (p RecoveryPlan) Validate(target Target) error {
	if p.Receipt != "" && !idPattern.MatchString(p.Receipt) {
		return errors.New("invalid recovery receipt identity")
	}
	if p.CandidateCommit != target.Artifact.Commit || p.BaselineCommit != target.Baseline.Commit || !digestPattern.MatchString(p.AssetsSHA256) || !digestPattern.MatchString(p.CompatibilitySHA256) || !p.ForwardCompatible || !p.BackwardCompatible {
		return errors.New("baseline assets and bidirectional database compatibility must be verified before installation")
	}
	return p.Baseline.Validate(target.Artifact.Kind, target.Baseline.Commit)
}

// restore runs without a backend/API probe: restoring that API is its purpose.
// Every side effect has a prior durable direction/checkpoint. Errors remain
// sticky until an explicit retry, while process termination leaves a resumable
// restoring checkpoint. A restored baseline never makes the update successful.
func (j Journal) restore(ctx context.Context, id string, r *Receipt, t *Target, driver Driver) error {
	if t.Recovery == nil || t.Recovery.CandidateError == "" {
		return errors.New("missing approved recovery snapshot")
	}
	if err := t.Recovery.Plan.Validate(*t); err != nil {
		return err
	}
	save := func() error { r.Targets[id] = *t; return j.save(*r) }
	fail := func(stage Phase, err error) error {
		var interrupted adapterInterruptedError
		if errors.As(err, &interrupted) {
			return err
		}
		if errors.Is(err, context.Canceled) {
			return err
		}
		t.Phase = Failed
		t.ResumePhase = stage
		t.Recovery.Status = "failed"
		t.Recovery.Error = err.Error()
		if saveErr := save(); saveErr != nil {
			return errors.Join(err, saveErr)
		}
		return err
	}
	if t.Phase == Restoring {
		t.Recovery.Status = "restoring"
		if err := save(); err != nil {
			return err
		}
		if err := driver.Rollback(ctx, *t); err != nil {
			return fail(Restoring, err)
		}
		t.Phase = VerifyingRestore
		t.Recovery.Status = "verifying"
		if err := save(); err != nil {
			return err
		}
	}
	if t.Phase != VerifyingRestore {
		return errors.New("invalid automatic restoration checkpoint")
	}
	observation, err := driver.VerifyRollback(ctx, *t)
	if err == nil {
		err = observation.Validate(t.Artifact.Kind, t.Baseline.Commit)
		if err == nil {
			err = observation.ValidateVersions(t.Recovery.Plan.Baseline.Versions())
		}
	}
	if err != nil {
		return fail(VerifyingRestore, err)
	}
	t.Observation = &observation
	t.Recovery.Status = "restored"
	t.Recovery.Error = ""
	t.Phase = Failed
	t.ResumePhase = Prepared
	t.Error = t.Recovery.CandidateError
	t.FailureStage = t.Recovery.CandidateStage
	if err := save(); err != nil {
		return err
	}
	return errors.New(t.Recovery.CandidateError)
}
