package handler

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/middleware"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// loadReadableTask shares the transcript access boundary with diagnostics.
func (h *Handler) loadReadableTask(w http.ResponseWriter, r *http.Request) (db.AgentTaskQueue, bool) {
	taskID := chi.URLParam(r, "taskId")
	taskUUID, ok := parseUUIDOrBadRequest(w, taskID, "task_id")
	if !ok {
		return db.AgentTaskQueue{}, false
	}

	task, err := h.Queries.GetAgentTask(r.Context(), taskUUID)
	if err != nil {
		if !isNotFound(err) {
			slog.Warn("get agent task failed", "task_id", taskID, "error", err)
			writeError(w, http.StatusInternalServerError, "failed to load task")
			return db.AgentTaskQueue{}, false
		}
		writeError(w, http.StatusNotFound, "task not found")
		return db.AgentTaskQueue{}, false
	}

	// Verify the task belongs to the caller's workspace. A failed lookup is a
	// 5xx here too: this endpoint does not drive the daemon's interrupt, but
	// telling a reader "this task does not exist" because the DB blinked is
	// the same lie, and it trains clients to give up on a retryable error.
	scope, err := h.TaskService.ResolveTaskScopeChecked(r.Context(), task)
	if err != nil {
		slog.Warn("resolve task workspace failed", "task_id", taskID, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to load task")
		return db.AgentTaskQueue{}, false
	}
	// Mismatch stays a 404 rather than a 403: a task in another workspace must
	// be indistinguishable from one that does not exist.
	wsID := scope.WorkspaceID
	if wsID == "" || wsID != middleware.WorkspaceIDFromContext(r.Context()) {
		writeError(w, http.StatusNotFound, "task not found")
		return db.AgentTaskQueue{}, false
	}
	// A chat task's transcript belongs to the chat session's creator alone;
	// to anyone else it does not exist either.
	if task.ChatSessionID.Valid && (scope.ChatSessionOwnerID == "" || scope.ChatSessionOwnerID != requestUserID(r)) {
		writeError(w, http.StatusNotFound, "task not found")
		return db.AgentTaskQueue{}, false
	}

	return task, true
}

// GetTaskRunDiagnostics is a single-task, payload-free read. An empty message
// store cannot prove that the provider never started or that reporting succeeded.
func (h *Handler) GetTaskRunDiagnostics(w http.ResponseWriter, r *http.Request) {
	task, ok := h.loadReadableTask(w, r)
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
