package handler

import (
	"net/http"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// GetTaskRunDiagnostics is a single-task, payload-free read. An empty message
// store cannot prove that the provider never started or that reporting succeeded.
func (h *Handler) GetTaskRunDiagnostics(w http.ResponseWriter, r *http.Request) {
	task, ok := h.loadTaskForUser(w, r)
	if !ok {
		return
	}
	rows, err := h.Queries.ListTaskMessageDiagnostics(r.Context(), task.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load task diagnostics")
		return
	}
	writeJSON(w, http.StatusOK, taskRunDiagnostics(task, rows))
}

func taskRunDiagnostics(task db.AgentTaskQueue, rows []db.ListTaskMessageDiagnosticsRow) protocol.TaskRunDiagnostics {
	response := protocol.TaskRunDiagnostics{
		Version: 1, TaskID: uuidToString(task.ID), IssueID: uuidToString(task.IssueID), Status: task.Status,
		StartedAt: timestampToPtr(task.StartedAt), CompletedAt: timestampToPtr(task.CompletedAt),
		SessionRolloutMissing: task.SessionRolloutMissing,
		ModelActivity:         "unknown", Persistence: "unknown", Messages: []protocol.TaskMessageDiagnostic{},
	}
	if task.SessionID.Valid {
		response.SessionID = &task.SessionID.String
	}
	if task.FailureReason.Valid {
		response.FailureReason = &task.FailureReason.String
	}
	for _, row := range rows {
		response.Persistence = "messages_present"
		response.Messages = append(response.Messages, protocol.TaskMessageDiagnostic{
			Type: row.Type, Count: row.Count, FirstSeq: row.FirstSeq,
			FirstCreatedAt: timestampToPtr(row.FirstCreatedAt),
		})
		// Only persisted model/tool events prove activity. Error/status-only
		// history and session pointers say nothing about model execution.
		if row.Type != "text" && row.Type != "thinking" && row.Type != "tool_use" && row.Type != "tool_result" {
			continue
		}
		if row.ActivityCount == 0 {
			continue
		}
		response.ModelActivity = "observed"
		if response.FirstActivitySeq == nil || row.FirstActivitySeq < *response.FirstActivitySeq {
			seq := row.FirstActivitySeq
			response.FirstActivitySeq = &seq
		}
		at := timestampToPtr(row.FirstActivityAt)
		if at != nil && (response.FirstActivityAt == nil || *at < *response.FirstActivityAt) {
			response.FirstActivityAt = at
		}
	}
	if response.ModelActivity == "observed" {
		response.Persistence = "model_activity_persisted"
	}
	return response
}
