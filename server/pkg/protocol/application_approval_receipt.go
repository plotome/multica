package protocol

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

// CardApprovalReceipt attests to an owner answer already verified by the card
// service. The service account is pinned separately by the HTTP handler.
type CardApprovalReceipt struct {
	DecisionID string `json:"decision_id"`
	OwnerID    string `json:"owner_id"`
	IssueID    string `json:"issue_id"`
	TaskID     string `json:"task_id"`
	RequestID  string `json:"request_id"`
	Choice     string `json:"choice"`
	ExpiresAt  string `json:"expires_at"`
	Signature  string `json:"signature"`
}

func (r CardApprovalReceipt) Valid(secret string, now time.Time) bool {
	if secret == "" {
		return false
	}
	fields := []string{r.DecisionID, r.OwnerID, r.IssueID, r.TaskID, r.RequestID, r.Choice, r.ExpiresAt}
	for _, field := range fields {
		if field == "" || strings.ContainsAny(field, "\r\n") {
			return false
		}
	}
	expires, err := time.Parse(time.RFC3339, r.ExpiresAt)
	if err != nil || !expires.After(now) {
		return false
	}
	sig, err := hex.DecodeString(r.Signature)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strings.Join(fields, "\n")))
	return hmac.Equal(sig, mac.Sum(nil))
}
