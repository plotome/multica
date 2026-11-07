package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func diagnosticsRequest(t *testing.T, userID, workspaceID, taskID string) *testutil.Response {
	t.Helper()
	req := withURLParam(newRequestAs(userID, http.MethodGet, "/api/tasks/"+taskID+"/diagnostics", nil), "taskId", taskID)
	req = req.WithContext(middleware.SetMemberContext(req.Context(), workspaceID, db.Member{}))
	return testutil.Call(t, testHandler.GetTaskRunDiagnostics, req)
}

func TestTaskRunDiagnosticsEmptyFailedHistoryIsUnknown(t *testing.T) {
	if testHandler == nil {
		t.Fatal("diagnostics integration requires the isolated test database")
	}
	agentID := createHandlerTestAgent(t, "diagnostics", []byte("[]"))
	issueID := dbfx.Issue(t, "diagnostics")
	for _, missing := range []bool{false, true} {
		taskID := dbfx.Task(t, agentID, testutil.Cols{
			"issue_id": issueID, "status": "failed", "started_at": testutil.Raw("now()"),
			"completed_at": testutil.Raw("now()"), "session_id": "session-pointer",
			"session_rollout_missing": missing,
			"failure_reason":          "agent_error.model_not_found_or_unavailable",
			"error":                   "Selected model is at capacity. Please try a different model.",
		})
		var got protocol.TaskRunDiagnostics
		diagnosticsRequest(t, testUserID, testWorkspaceID, taskID).Want(http.StatusOK).JSON(&got)
		if got.ModelActivity != "unknown" || got.Persistence != "unknown" ||
			got.FirstActivitySeq != nil || got.FirstActivityAt != nil || got.Messages == nil || len(got.Messages) != 0 {
			t.Fatalf("empty failed history must remain unknown: %+v", got)
		}
		if got.SessionID == nil || *got.SessionID != "session-pointer" || got.SessionRolloutMissing != missing {
			t.Fatalf("stored session evidence was lost: %+v", got)
		}
	}
}

func TestTaskRunDiagnosticsReportsActivityWithoutPayloads(t *testing.T) {
	if testHandler == nil {
		t.Fatal("diagnostics integration requires the isolated test database")
	}
	taskID := seedBatchTask(t, "diagnostics-activity")
	add := func(seq int, kind, content string) {
		dbfx.Insert(t, "task_message", testutil.Cols{
			"task_id": taskID, "seq": seq, "type": kind, "content": content,
			"output": "PRIVATE_OUTPUT", "input": testutil.Raw(`'{"private":"PRIVATE_INPUT"}'::jsonb`),
		})
	}
	add(1, "error", "PRIVATE_ERROR")
	add(2, "text", "")
	var got protocol.TaskRunDiagnostics
	diagnosticsRequest(t, testUserID, testWorkspaceID, taskID).Want(http.StatusOK).JSON(&got)
	if got.ModelActivity != "unknown" || got.Persistence != "messages_present" || got.FirstActivitySeq != nil {
		t.Fatalf("error and empty text must not prove model execution: %+v", got)
	}
	add(3, "tool_use", "PRIVATE_TOOL")
	add(4, "tool_result", "PRIVATE_RESULT")
	add(5, "text", "PRIVATE_TEXT")
	diagnosticsRequest(t, testUserID, testWorkspaceID, taskID).Want(http.StatusOK).JSON(&got)
	if got.Version != 1 || got.ModelActivity != "observed" || got.Persistence != "model_activity_persisted" ||
		got.FirstActivitySeq == nil || *got.FirstActivitySeq != 3 || got.FirstActivityAt == nil {
		t.Fatalf("missing persisted activity evidence: %+v", got)
	}
	if len(got.Messages) != 4 || got.Messages[1].Type != "text" || got.Messages[1].Count != 2 || got.Messages[1].FirstSeq != 2 {
		t.Fatalf("incorrect aggregate: %+v", got.Messages)
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "PRIVATE_") || strings.Contains(string(raw), "\"content\"") || strings.Contains(string(raw), "\"output\"") {
		t.Fatalf("diagnostics leaked message payload: %s", raw)
	}
}

func TestTaskRunDiagnosticsSharesTranscriptAccess(t *testing.T) {
	if testHandler == nil {
		t.Fatal("diagnostics integration requires the isolated test database")
	}
	f := seedChatTaskPrivacy(t)
	diagnosticsRequest(t, f.ownerID, testWorkspaceID, f.chatTaskID).Want(http.StatusOK)
	diagnosticsRequest(t, testUserID, testWorkspaceID, f.chatTaskID).Want(http.StatusNotFound)
	diagnosticsRequest(t, testUserID, testWorkspaceID, f.issueTaskID).Want(http.StatusOK)
	diagnosticsRequest(t, testUserID, "00000000-0000-0000-0000-000000000001", f.issueTaskID).Want(http.StatusNotFound)
	diagnosticsRequest(t, testUserID, testWorkspaceID, "00000000-0000-0000-0000-000000000001").Want(http.StatusNotFound)
}

func TestTaskRunDiagnosticsInvalidTaskID(t *testing.T) {
	req := withURLParam(httptest.NewRequest(http.MethodGet, "/", nil), "taskId", "invalid")
	w := httptest.NewRecorder()
	(&Handler{}).GetTaskRunDiagnostics(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid UUID: status=%d", w.Code)
	}
}
