package protocol

// TaskRunDiagnostics describes persisted evidence, never inferred non-execution.
type TaskRunDiagnostics struct {
	Version               int                     `json:"version"`
	TaskID                string                  `json:"task_id"`
	IssueID               string                  `json:"issue_id,omitempty"`
	Status                string                  `json:"status"`
	StartedAt             *string                 `json:"started_at"`
	CompletedAt           *string                 `json:"completed_at"`
	SessionID             *string                 `json:"session_id"`
	SessionRolloutMissing bool                    `json:"session_rollout_missing"`
	FailureReason         *string                 `json:"failure_reason"`
	ModelActivity         string                  `json:"model_activity"`
	Persistence           string                  `json:"persistence"`
	FirstActivitySeq      *int32                  `json:"first_activity_seq"`
	FirstActivityAt       *string                 `json:"first_activity_at"`
	Messages              []TaskMessageDiagnostic `json:"messages"`
}

type TaskMessageDiagnostic struct {
	Type           string  `json:"type"`
	Count          int64   `json:"count"`
	FirstSeq       int32   `json:"first_seq"`
	FirstCreatedAt *string `json:"first_created_at"`
}
