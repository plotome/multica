package handler

import (
	"encoding/json"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/middleware"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
	"net/http"
	"os"
	"time"
)

type taskElicitationResponse struct {
	ID        string                                `json:"id"`
	Status    string                                `json:"status"`
	Request   json.RawMessage                       `json:"request"`
	Response  json.RawMessage                       `json:"response,omitempty"`
	Approval  *protocol.ApplicationApprovalRequired `json:"approval,omitempty"`
	ExpiresAt *time.Time                            `json:"expires_at,omitempty"`
	IssueID   string                                `json:"issue_id,omitempty"`
}

func elicitationResponse(e db.TaskElicitation) taskElicitationResponse {
	r := taskElicitationResponse{ID: uuidToString(e.ID), Status: e.Status, Request: e.Request, Response: e.Response}
	if e.ExpiresAt.Valid {
		r.ExpiresAt = &e.ExpiresAt.Time
	}
	return r
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
	p, err := protocol.ParseMCPElicitation(req.Request)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if !task.IssueID.Valid {
		writeError(w, 409, "deferred application approval requires an issue")
		return
	}
	ttl, err := time.ParseDuration(os.Getenv("COMPUTER_USE_APPROVAL_TTL"))
	if err != nil || ttl <= 0 {
		writeError(w, 503, "COMPUTER_USE_APPROVAL_TTL must be explicitly configured as a positive duration")
		return
	}
	scopes, _ := json.Marshal(p.Meta.Persist)
	grant, err := h.Queries.ConsumeTaskElicitation(r.Context(), db.ConsumeTaskElicitationParams{TaskID: task.ID, WorkspaceID: parseUUID(ws), App: p.SubjectKey(), OfferedScopes: scopes})
	if err == nil {
		writeJSON(w, 200, elicitationResponse(grant))
		return
	}
	if !isNotFound(err) {
		writeError(w, 500, "failed to consume application approval")
		return
	}
	e, err := h.Queries.FindUnconsumedTaskElicitation(r.Context(), db.FindUnconsumedTaskElicitationParams{TaskID: task.ID, WorkspaceID: parseUUID(ws), App: p.SubjectKey()})
	if err != nil && !isNotFound(err) {
		writeError(w, 500, "failed to load application approval")
		return
	}
	if isNotFound(err) {
		e, err = h.Queries.CreateTaskElicitation(r.Context(), db.CreateTaskElicitationParams{ID: id, TaskID: task.ID, WorkspaceID: parseUUID(ws), Request: req.Request, ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(ttl), Valid: true}})
	}
	if err != nil {
		if isNotFound(err) {
			writeError(w, 409, "task is no longer running")
		} else {
			writeError(w, 500, "failed to create approval")
		}
		return
	}
	result := elicitationResponse(e)
	code := "computer_use_approval_required"
	if !e.ExpiresAt.Time.After(time.Now()) {
		code = "computer_use_approval_expired"
	} else if e.Status != "pending" {
		code = "computer_use_approval_denied"
	}
	result.Approval = &protocol.ApplicationApprovalRequired{Code: code, RequestID: uuidToString(e.ID), TaskID: uuidToString(e.TaskID), IssueID: uuidToString(task.IssueID), App: p.Resource(), Request: e.Request, ExpiresAt: e.ExpiresAt.Time.Format(time.RFC3339Nano)}
	// A previously answered request with incompatible scopes is never forwarded.
	result.Response = nil
	writeJSON(w, http.StatusCreated, result)
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
	return h.requireElicitationActor(w, r, nil, false)
}

func (h *Handler) requireElicitationActor(w http.ResponseWriter, r *http.Request, receipt *protocol.CardApprovalReceipt, serviceRead bool) (db.AgentTaskQueue, pgtype.UUID, bool) {
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
	actor := requestUserID(r)
	service := os.Getenv("COMPUTER_USE_CARD_SERVICE_USER_ID")
	if actor != "" && actor == service {
		if serviceRead {
			actor = uuidToString(rt.OwnerID)
		} else if receipt != nil && receipt.Valid(os.Getenv("COMPUTER_USE_CARD_SECRET"), time.Now()) && receipt.TaskID == uuidToString(task.ID) && receipt.RequestID == chi.URLParam(r, "requestId") && receipt.IssueID == uuidToString(task.IssueID) {
			actor = receipt.OwnerID
		}
	}
	if actor == "" || uuidToString(rt.OwnerID) != actor {
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

func (h *Handler) GetTaskElicitationForOwner(w http.ResponseWriter, r *http.Request) {
	task, ws, ok := h.requireElicitationActor(w, r, nil, true)
	if !ok {
		return
	}
	id, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "requestId"), "request_id")
	if !ok {
		return
	}
	e, err := h.Queries.GetTaskElicitation(r.Context(), db.GetTaskElicitationParams{ID: id, TaskID: task.ID, WorkspaceID: ws})
	if err != nil {
		writeError(w, 404, "approval not found")
		return
	}
	result := elicitationResponse(e)
	result.IssueID = uuidToString(task.IssueID)
	writeJSON(w, 200, result)
}
func (h *Handler) RespondTaskElicitation(w http.ResponseWriter, r *http.Request) {
	if isMachineCredentialActor(r) {
		writeError(w, 403, "application approvals require the human device owner")
		return
	}
	var req struct {
		Choice  string                        `json:"choice"`
		Receipt *protocol.CardApprovalReceipt `json:"card_receipt,omitempty"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil {
		writeError(w, 400, "invalid choice")
		return
	}
	if req.Receipt != nil && req.Receipt.Choice != req.Choice {
		writeError(w, 400, "card choice mismatch")
		return
	}
	task, ws, ok := h.requireElicitationActor(w, r, req.Receipt, false)
	if !ok {
		return
	}
	id, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "requestId"), "request_id")
	if !ok {
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
	ownerID := requestUserID(r)
	if req.Receipt != nil && ownerID == os.Getenv("COMPUTER_USE_CARD_SERVICE_USER_ID") {
		ownerID = req.Receipt.OwnerID
	}
	_, err = h.Queries.RespondTaskElicitation(r.Context(), db.RespondTaskElicitationParams{ID: id, TaskID: task.ID, WorkspaceID: ws, OwnerID: parseUUID(ownerID), Response: response})
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
