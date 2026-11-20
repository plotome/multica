package forkupdate

import "testing"

func TestControlApprovalHostIsolationAndStaleResult(t *testing.T) {
	c := verifiedFixture(t)
	r, err := Plan(c, inventoryFixture(c))
	if err != nil {
		t.Fatal(err)
	}
	s := ControlState{Receipt: r, Revision: 1}
	report := r.Targets["mini-server"]
	report.Phase = Succeeded
	observation := fixtureObservation(report, report.Artifact.Commit)
	report.Observation = &observation
	report.RollbackAvailable = true
	if err := s.ApplyReport("mini", "mini-server", report); err == nil {
		t.Fatal("unapproved report accepted")
	}
	if err := s.Action("approve", "wrong", ""); err == nil {
		t.Fatal("approved moving candidate")
	}
	if err := s.Action("approve", r.Digest, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyReport("air", "mini-server", report); err == nil {
		t.Fatal("another host reported success")
	}
	if err := s.ApplyReport("mini", "mini-server", report); err != nil {
		t.Fatal(err)
	}
	if s.Receipt.Complete() {
		t.Fatal("partial rollout called complete")
	}
	if err := s.Action("rollback", r.Digest, "mini-server"); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyReport("mini", "mini-server", report); err == nil {
		t.Fatal("late success overwrote rollback request")
	}
	current := s.Receipt.Targets["mini-server"]
	current.Phase = Failed
	current.ResumePhase = RollingBack
	current.FailureStage = "probe"
	current.Error = "unavailable"
	if err := s.ApplyReport("mini", "mini-server", current); err != nil {
		t.Fatal(err)
	}
	if err := s.Action("retry", r.Digest, "mini-server"); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyReport("mini", "mini-server", current); err == nil {
		t.Fatal("late failed report overwrote retry")
	}
	if s.Receipt.Targets["mini-server"].Operation != "rollback" {
		t.Fatal("control retry reversed operation")
	}
}
