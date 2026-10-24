package protocol

import "encoding/json"

// ApplicationApprovalRequired is delivered to the active model turn as well as
// the transcript. It never becomes an MCP accept response.
type ApplicationApprovalRequired struct {
	Code      string          `json:"code"`
	RequestID string          `json:"request_id"`
	TaskID    string          `json:"task_id"`
	IssueID   string          `json:"issue_id"`
	App       string          `json:"app"`
	Request   json.RawMessage `json:"request"`
	ExpiresAt string          `json:"expires_at"`
}

func (e *ApplicationApprovalRequired) Error() string {
	b, _ := json.Marshal(e)
	return string(b)
}
