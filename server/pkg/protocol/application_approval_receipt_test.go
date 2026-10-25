package protocol

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

func TestCardApprovalReceiptBindsOwnerTargetScopeAndExpiry(t *testing.T) {
	now := time.Now().UTC()
	r := CardApprovalReceipt{DecisionID: "P-1", OwnerID: "owner", IssueID: "issue", TaskID: "task", RequestID: "request", Choice: "once", ExpiresAt: now.Add(time.Hour).Format(time.RFC3339)}
	mac := hmac.New(sha256.New, []byte("fixture-secret"))
	mac.Write([]byte(strings.Join([]string{r.DecisionID, r.OwnerID, r.IssueID, r.TaskID, r.RequestID, r.Choice, r.ExpiresAt}, "\n")))
	r.Signature = hex.EncodeToString(mac.Sum(nil))
	if !r.Valid("fixture-secret", now) {
		t.Fatal("valid receipt rejected")
	}
	for _, mutate := range []func(*CardApprovalReceipt){
		func(r *CardApprovalReceipt) { r.Choice = "always" }, func(r *CardApprovalReceipt) { r.OwnerID = "agent" },
		func(r *CardApprovalReceipt) { r.RequestID = "other" }, func(r *CardApprovalReceipt) { r.IssueID = "other" },
		func(r *CardApprovalReceipt) { r.TaskID = "other" }, func(r *CardApprovalReceipt) { r.DecisionID = "P-2" },
	} {
		changed := r
		mutate(&changed)
		if changed.Valid("fixture-secret", now) {
			t.Fatal("tampered receipt accepted")
		}
	}
	if r.Valid("", now) || r.Valid("wrong-secret", now) || r.Valid("fixture-secret", now.Add(2*time.Hour)) {
		t.Fatal("unconfigured or expired receipt accepted")
	}
}
