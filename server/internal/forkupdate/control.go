package forkupdate

import (
	"encoding/json"
	"errors"
	"reflect"
)

// ControlState is persisted by the server, separately from each host's local
// journal. The signed envelope travels unchanged to every consumer.
type ControlState struct {
	Envelope json.RawMessage `json:"envelope"`
	Receipt  Receipt         `json:"receipt"`
	Revision int64           `json:"revision"`
}

func (s *ControlState) Action(action, digest, id string) error {
	if digest != s.Receipt.Digest {
		return errors.New("candidate changed; refresh update plan")
	}
	switch action {
	case "approve":
		s.Receipt.Approved = true
		return nil
	case "retry":
		return RetryTarget(&s.Receipt, id)
	case "rollback":
		return RequestTargetRollback(&s.Receipt, id)
	default:
		return errors.New("unknown update action")
	}
}

// ApplyReport excludes other hosts, immutable metadata edits, and stale results
// from an earlier retry or rollback command. Transport authorizes host identity.
func (s *ControlState) ApplyReport(host, id string, report Target) error {
	target, ok := s.Receipt.Targets[id]
	if !ok || report.CandidateDigest != s.Receipt.Digest || target.CandidateDigest != report.CandidateDigest || target.Artifact.Host != host || !reflect.DeepEqual(report.Artifact, target.Artifact) || report.Baseline != target.Baseline || report.Attempt != target.Attempt || report.Operation != target.Operation {
		return errors.New("report is not for the approved host, candidate and operation")
	}
	if !s.Receipt.Approved {
		if report.Operation != "install" || report.Phase != Prepared && report.Phase != Downloading && report.Phase != Validating && report.Phase != Waiting && report.Phase != Failed {
			return errors.New("unapproved operation may only prepare a package")
		}
		if report.Phase == Failed && report.ResumePhase != Prepared {
			return errors.New("unapproved operation cannot fail after installation")
		}
	}
	switch report.Phase {
	case Prepared, Downloading, Validating, Waiting, Installing, Verifying, Succeeded, Failed, RollingBack, VerifyingRollback, RolledBack, Restoring, VerifyingRestore:
	default:
		return errors.New("unknown reported update phase")
	}
	if report.Operation == "rollback" && report.Phase != RollingBack && report.Phase != VerifyingRollback && report.Phase != RolledBack && report.Phase != Failed {
		return errors.New("rollback report changed direction")
	}
	if report.Operation == "install" && (report.Phase == RollingBack || report.Phase == VerifyingRollback || report.Phase == RolledBack) {
		return errors.New("install report changed direction")
	}
	if report.Phase == Failed && (report.FailureStage == "" || report.ResumePhase == "") {
		return errors.New("failed report must retain its checkpoint")
	}
	if report.Recovery != nil {
		if err := report.Recovery.Plan.Validate(report); err != nil {
			return err
		}
		switch report.Recovery.Status {
		case "prepared", "restoring", "verifying", "restored", "failed":
		default:
			return errors.New("unknown recovery result")
		}
		if report.Recovery.Status == "restored" {
			if report.Phase != Failed || report.Recovery.CandidateError == "" || report.Observation == nil {
				return errors.New("restoration must retain candidate failure and running baseline")
			}
			if err := report.Observation.Validate(report.Artifact.Kind, report.Baseline.Commit); err != nil {
				return err
			}
			if err := report.Observation.ValidateVersions(report.Recovery.Plan.Baseline.Versions()); err != nil {
				return err
			}
		}
		if target.Recovery != nil && target.Phase != Prepared && target.Phase != Waiting && !reflect.DeepEqual(target.Recovery.Plan, report.Recovery.Plan) {
			return errors.New("recovery baseline changed after installation started")
		}
	}
	if report.Phase == Restoring || report.Phase == VerifyingRestore {
		if report.Recovery == nil || report.Recovery.CandidateError == "" {
			return errors.New("candidate failure and approved recovery snapshot required")
		}
	}
	if report.Phase == Succeeded || report.Phase == RolledBack {
		commit := report.Artifact.Commit
		if report.Phase == RolledBack {
			commit = report.Baseline.Commit
		}
		if report.Observation == nil {
			return errors.New("running observation required")
		}
		if err := report.Observation.Validate(report.Artifact.Kind, commit); err != nil {
			return err
		}
		if report.Phase == Succeeded {
			if err := report.Observation.ValidateVersions(report.Artifact.ComponentVersions); err != nil {
				return err
			}
		}
	}
	if (target.Phase == Succeeded || target.Phase == RolledBack || target.Phase == Failed) && !reflect.DeepEqual(report, target) {
		return errors.New("terminal operation requires a new explicit action")
	}
	s.Receipt.Targets[id] = report
	return nil
}
