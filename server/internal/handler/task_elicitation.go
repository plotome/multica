package handler

import (
	"encoding/json"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/middleware"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
	"net/http"
)

type taskElicitationResponse struct {
	ID       string          `json:"id"`
	Status   string          `json:"status"`
	Request  json.RawMessage `json:"request"`
	Response json.RawMessage `json:"response,omitempty"`
}

func elicitationResponse(e db.TaskElicitation) taskElicitationResponse {
	return taskElicitationResponse{ID: uuidToString(e.ID), Status: e.Status, Request: e.Request, Response: e.Response}
}

func (h *Handler) CreateTaskElicitation(w http.ResponseWriter, r *http.Request) {
	task, ws, ok := h.requireDaemonTaskAccessWithWorkspace(w, r, chi.URLParam(r, "taskId"))
	if !ok {
		return
	}
	var req struct {
		ID      string          `json:"id"`
		Request json.RawMessage `json:"request"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil {
		writeError(w, 400, "invalid request")
		return
	}
	id, ok := parseUUIDOrBadRequest(w, req.ID, "id")
	if !ok {
		return
	}
	if _, err := protocol.ParseMCPElicitation(req.Request); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	e, err := h.Queries.CreateTaskElicitation(r.Context(), db.CreateTaskElicitationParams{ID: id, TaskID: task.ID, WorkspaceID: parseUUID(ws), Request: req.Request})
	if err != nil {
		if isNotFound(err) {
			writeError(w, 409, "task is no longer running")
		} else {
			writeError(w, 500, "failed to create approval")
		}
		return
	}
	writeJSON(w, http.StatusCreated, elicitationResponse(e))
}
func (h *Handler) GetTaskElicitation(w http.ResponseWriter, r *http.Request) {
	task, ws, ok := h.requireDaemonTaskAccessWithWorkspace(w, r, chi.URLParam(r, "taskId"))
	if !ok {
		return
	}
	id, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "requestId"), "request_id")
	if !ok {
		return
	}
	e, err := h.Queries.GetTaskElicitation(r.Context(), db.GetTaskElicitationParams{ID: id, TaskID: task.ID, WorkspaceID: parseUUID(ws)})
	if err != nil {
		if isNotFound(err) {
			writeError(w, 410, "approval is no longer active")
		} else {
			writeError(w, 500, "failed to load approval")
		}
		return
	}
	writeJSON(w, 200, elicitationResponse(e))
}
func (h *Handler) CancelTaskElicitation(w http.ResponseWriter, r *http.Request) {
	task, ws, ok := h.requireDaemonTaskAccessWithWorkspace(w, r, chi.URLParam(r, "taskId"))
	if !ok {
		return
	}
	id, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "requestId"), "request_id")
	if !ok {
		return
	}
	if err := h.Queries.CancelTaskElicitation(r.Context(), db.CancelTaskElicitationParams{ID: id, TaskID: task.ID, WorkspaceID: parseUUID(ws)}); err != nil {
		writeError(w, 500, "failed to cancel approval")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// The device owner is the authority for local application access. Membership,
// agent ownership, administrative role and task tokens do not confer this right.
func (h *Handler) requireElicitationOwner(w http.ResponseWriter, r *http.Request) (db.AgentTaskQueue, pgtype.UUID, bool) {
	if isMachineCredentialActor(r) {
		writeError(w, 403, "application approvals require the human device owner")
		return db.AgentTaskQueue{}, pgtype.UUID{}, false
	}
	id, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "taskId"), "task_id")
	if !ok {
		return db.AgentTaskQueue{}, pgtype.UUID{}, false
	}
	task, err := h.Queries.GetAgentTask(r.Context(), id)
	if err != nil {
		writeError(w, 404, "task not found")
		return task, pgtype.UUID{}, false
	}
	ws, err := h.TaskService.ResolveTaskWorkspaceIDChecked(r.Context(), task)
	if err != nil {
		writeError(w, 500, "failed to load task")
		return task, pgtype.UUID{}, false
	}
	if ws == "" || ws != middleware.WorkspaceIDFromContext(r.Context()) || !task.RuntimeID.Valid {
		writeError(w, 404, "task not found")
		return task, pgtype.UUID{}, false
	}
	rt, err := h.Queries.GetAgentRuntime(r.Context(), task.RuntimeID)
	if err != nil {
		writeError(w, 500, "failed to load device owner")
		return task, pgtype.UUID{}, false
	}
	if requestUserID(r) == "" || uuidToString(rt.OwnerID) != requestUserID(r) {
		writeError(w, 403, "only the device owner can review application approvals")
		return task, pgtype.UUID{}, false
	}
	return task, parseUUID(ws), true
}
func (h *Handler) ListTaskElicitations(w http.ResponseWriter, r *http.Request) {
	task, ws, ok := h.requireElicitationOwner(w, r)
	if !ok {
		return
	}
	rows, err := h.Queries.ListTaskElicitations(r.Context(), db.ListTaskElicitationsParams{TaskID: task.ID, WorkspaceID: ws})
	if err != nil {
		writeError(w, 500, "failed to list approvals")
		return
	}
	out := make([]taskElicitationResponse, 0, len(rows))
	for _, e := range rows {
		out = append(out, elicitationResponse(e))
	}
	writeJSON(w, 200, out)
}
func (h *Handler) RespondTaskElicitation(w http.ResponseWriter, r *http.Request) {
	task, ws, ok := h.requireElicitationOwner(w, r)
	if !ok {
		return
	}
	id, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "requestId"), "request_id")
	if !ok {
		return
	}
	var req struct {
		Choice string `json:"choice"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil {
		writeError(w, 400, "invalid choice")
		return
	}
	e, err := h.Queries.GetTaskElicitation(r.Context(), db.GetTaskElicitationParams{ID: id, TaskID: task.ID, WorkspaceID: ws})
	if err != nil {
		if isNotFound(err) {
			writeError(w, 409, "approval is no longer active")
		} else {
			writeError(w, 500, "failed to load approval")
		}
		return
	}
	response, err := protocol.ElicitationResponse(e.Request, req.Choice)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	_, err = h.Queries.RespondTaskElicitation(r.Context(), db.RespondTaskElicitationParams{ID: id, TaskID: task.ID, WorkspaceID: ws, OwnerID: parseUUID(requestUserID(r)), Response: response})
	if err != nil {
		if isNotFound(err) {
			writeError(w, 409, "approval is no longer active")
		} else {
			writeError(w, 500, "failed to respond to approval")
		}
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
