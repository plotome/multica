package handler

import (
	"encoding/json"
	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"net/http"
	"testing"
)

const handlerApproval = `{"threadId":"thread","turnId":"turn","serverName":"cua_repl","mode":"form","message":"Allow Fixture?","requestedSchema":{"type":"object","properties":{}},"_meta":{"codex_approval_kind":"mcp_tool_call","connector_id":"computer-use","tool_params":{"app":"fixture.app"},"persist":["session"]}}`

func newApprovalFixture(t *testing.T) (supplementFixture, string) {
	t.Helper()
	f := newSupplementFixture(t, "codex", "running", false)
	id := uuid.NewString()
	dbfx.Cleanup(t, `DELETE FROM task_elicitation WHERE task_id=$1`, f.taskID)
	req := withURLParam(newOwnerDaemonRequest("POST", "/", map[string]any{"id": id, "request": json.RawMessage(handlerApproval)}), "taskId", f.taskID)
	testutil.Call(t, testHandler.CreateTaskElicitation, req).Want(201)
	return f, id
}
func approvalHumanRequest(taskID, id, choice string) *http.Request {
	req := withURLParams(newRequest("POST", "/", map[string]string{"choice": choice}), "taskId", taskID, "requestId", id)
	return req.WithContext(middleware.SetMemberContext(req.Context(), testWorkspaceID, db.Member{}))
}
func TestTaskElicitationOwnerOnlyReplayAndScope(t *testing.T) {
	f, id := newApprovalFixture(t)
	req := approvalHumanRequest(f.taskID, id, "once")
	req.Header.Set("X-Actor-Source", "task_token")
	testutil.Call(t, testHandler.RespondTaskElicitation, req).Want(403)
	req = approvalHumanRequest(f.taskID, id, "once")
	req.Header.Set("X-User-ID", uuid.NewString())
	testutil.Call(t, testHandler.RespondTaskElicitation, req).Want(403)
	req = approvalHumanRequest(f.taskID, id, "once")
	req = req.WithContext(middleware.SetMemberContext(req.Context(), uuid.NewString(), db.Member{}))
	testutil.Call(t, testHandler.RespondTaskElicitation, req).Want(404)
	testutil.Call(t, testHandler.RespondTaskElicitation, approvalHumanRequest(f.taskID, id, "always")).Want(400)
	testutil.Call(t, testHandler.RespondTaskElicitation, approvalHumanRequest(f.taskID, id, "session")).Want(200)
	testutil.Call(t, testHandler.RespondTaskElicitation, approvalHumanRequest(f.taskID, id, "once")).Want(409)
	var response taskElicitationResponse
	testutil.Call(t, testHandler.GetTaskElicitation, withURLParams(newOwnerDaemonRequest("GET", "/", nil), "taskId", f.taskID, "requestId", id)).Want(200).JSON(&response)
	assertJSONEqual(t, response.Response, `{"action":"accept","content":{},"_meta":{"persist":"session"}}`)
	other := newSupplementFixture(t, "claude", "running", false)
	testutil.Call(t, testHandler.GetTaskElicitation, withURLParams(newOwnerDaemonRequest("GET", "/", nil), "taskId", other.taskID, "requestId", id)).Want(410)
	testutil.Call(t, testHandler.GetTaskElicitation, withURLParams(newDaemonTokenRequest("GET", "/", nil, testWorkspaceID, "different-device"), "taskId", f.taskID, "requestId", id)).Want(404)
}
func TestTaskElicitationCancellationAndTerminal(t *testing.T) {
	for _, end := range []string{"cancel", "complete"} {
		t.Run(end, func(t *testing.T) {
			f, id := newApprovalFixture(t)
			if end == "cancel" {
				testutil.Call(t, testHandler.CancelTaskElicitation, withURLParams(newOwnerDaemonRequest("POST", "/", nil), "taskId", f.taskID, "requestId", id)).Want(200)
			} else {
				w := completeTaskViaHandler(t, f.taskID, "done")
				if w.Code != 200 {
					t.Fatalf("complete: %d %s", w.Code, w.Body.String())
				}
				if n := dbfx.Count(t, `SELECT count(*) FROM task_elicitation WHERE task_id=$1`, f.taskID); n != 0 {
					t.Fatal("terminal task retained approvals")
				}
			}
			testutil.Call(t, testHandler.RespondTaskElicitation, approvalHumanRequest(f.taskID, id, "once")).Want(409)
			var approvals []taskElicitationResponse
			testutil.Call(t, testHandler.ListTaskElicitations, approvalHumanRequest(f.taskID, id, "")).Want(200).JSON(&approvals)
			if len(approvals) != 0 {
				t.Fatal("stale approval visible")
			}
		})
	}
}
func TestAgentCreateRuntimeManifestMachineDenied(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database is required")
	}
	req := newRequest("POST", "/", map[string]any{"name": "must not create", "custom_env": map[string]string{"CODEX_RUNTIME_ENV_MANIFEST": `{"schema":1,"id":"self-grant","names":["LATE_PATH"]}`}})
	req.Header.Set("X-Actor-Source", "task_token")
	testutil.Call(t, testHandler.CreateAgent, req).Want(403)
}
