package handler

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
	"net/http"
	"strings"
	"testing"
	"time"
)

const handlerApproval = `{"threadId":"thread","turnId":"turn","serverName":"cua_repl","mode":"form","message":"Allow Fixture?","requestedSchema":{"type":"object","properties":{}},"_meta":{"codex_approval_kind":"mcp_tool_call","connector_id":"computer-use","tool_params":{"app":"fixture.app"},"persist":["session"]}}`

func newApprovalFixture(t *testing.T) (supplementFixture, string) {
	t.Helper()
	t.Setenv("COMPUTER_USE_APPROVAL_TTL", "1h")
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
func TestTaskElicitationSurvivesTerminalAndConsumesOnce(t *testing.T) {
	f, id := newApprovalFixture(t)
	if w := completeTaskViaHandler(t, f.taskID, "done"); w.Code != 200 {
		t.Fatal(w.Code)
	}
	testutil.Call(t, testHandler.RespondTaskElicitation, approvalHumanRequest(f.taskID, id, "once")).Want(200)
	retry := dbfx.Task(t, f.agentID, testutil.Cols{"status": "running", "issue_id": f.issueID, "runtime_id": f.runtimeID})
	dbfx.Cleanup(t, `DELETE FROM task_elicitation WHERE task_id=$1`, retry)
	create := func() taskElicitationResponse {
		t.Helper()
		var response taskElicitationResponse
		req := withURLParam(newOwnerDaemonRequest("POST", "/", map[string]any{"id": uuid.NewString(), "request": json.RawMessage(handlerApproval)}), "taskId", retry)
		w := testutil.Call(t, testHandler.CreateTaskElicitation, req)
		w.JSON(&response)
		return response
	}
	first := create()
	assertJSONEqual(t, first.Response, `{"action":"accept","content":{}}`)
	second := create()
	if second.Approval == nil || second.Approval.Code != "computer_use_approval_required" {
		t.Fatalf("reused once: %+v", second)
	}
}

func TestTaskElicitationExpiryAndDenialNeverGrant(t *testing.T) {
	for _, mode := range []string{"expired", "declined", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			f, id := newApprovalFixture(t)
			if mode == "expired" {
				dbfx.Exec(t, `UPDATE task_elicitation SET expires_at=now()-interval '1 second' WHERE id=$1`, id)
			} else {
				choice := "decline"
				if mode == "cancelled" {
					choice = "cancel"
				}
				testutil.Call(t, testHandler.RespondTaskElicitation, approvalHumanRequest(f.taskID, id, choice)).Want(200)
			}
			var result taskElicitationResponse
			req := withURLParam(newOwnerDaemonRequest("POST", "/", map[string]any{"id": uuid.NewString(), "request": json.RawMessage(handlerApproval)}), "taskId", f.taskID)
			testutil.Call(t, testHandler.CreateTaskElicitation, req).Want(201).JSON(&result)
			if result.Approval == nil || result.Approval.Code == "computer_use_approval_required" || len(result.Response) > 0 {
				t.Fatalf("unexpected grant %+v", result)
			}
			testutil.Call(t, testHandler.RespondTaskElicitation, approvalHumanRequest(f.taskID, id, "once")).Want(409)
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

func TestWorkspaceDeleteRemovesTaskElicitationsAndPreservesNeighbor(t *testing.T) {
	f := newWorkspaceDeletePathFixture(t, "elicitations")
	dbfx.Cleanup(t, `DELETE FROM task_elicitation WHERE workspace_id = ANY($1::uuid[])`, []string{f.victimID, f.neighbourID})
	// Cover each task ownership path, an orphan owned by the workspace, and
	// an approval belonging entirely to the neighboring workspace.
	for _, row := range []struct{ task, workspace string }{
		{f.taskViaAgent, f.neighbourID},
		{f.taskViaIssue, f.neighbourID},
		{f.taskViaRuntime, f.neighbourID},
		{uuid.NewString(), f.victimID},
		{f.neighbourTask, f.neighbourID},
	} {
		dbfx.Exec(t, `INSERT INTO task_elicitation (id, task_id, workspace_id, request, status)
VALUES (gen_random_uuid(), $1, $2, '{}'::jsonb, 'pending')`, row.task, row.workspace)
	}
	req := withURLParam(newRequest(http.MethodDelete, "/api/workspaces/"+f.victimID, nil), "id", f.victimID)
	testutil.Call(t, testHandler.DeleteWorkspace, req).Want(http.StatusNoContent)
	if n := dbfx.Count(t, `SELECT count(*) FROM task_elicitation WHERE workspace_id = $1 OR task_id = ANY($2::uuid[])`,
		f.victimID, []string{f.taskViaAgent, f.taskViaIssue, f.taskViaRuntime}); n != 0 {
		t.Fatalf("deleted workspace retained %d application approvals", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM task_elicitation WHERE task_id = $1`, f.neighbourTask); n != 1 {
		t.Fatalf("neighbor retained %d approvals, want 1", n)
	}
}

func TestTaskElicitationCardServiceRequiresOwnerReceiptAndRejectsTaskToken(t *testing.T) {
	f, id := newApprovalFixture(t)
	serviceID := uuid.NewString()
	t.Setenv("COMPUTER_USE_CARD_SERVICE_USER_ID", serviceID)
	t.Setenv("COMPUTER_USE_CARD_SECRET", "fixture-secret")
	receipt := protocol.CardApprovalReceipt{DecisionID: "P-1", OwnerID: testUserID, IssueID: f.issueID, TaskID: f.taskID, RequestID: id, Choice: "once", ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}
	mac := hmac.New(sha256.New, []byte("fixture-secret"))
	mac.Write([]byte(strings.Join([]string{receipt.DecisionID, receipt.OwnerID, receipt.IssueID, receipt.TaskID, receipt.RequestID, receipt.Choice, receipt.ExpiresAt}, "\n")))
	receipt.Signature = hex.EncodeToString(mac.Sum(nil))
	request := func(r *protocol.CardApprovalReceipt) *http.Request {
		req := withURLParams(newRequest("POST", "/", map[string]any{"choice": "once", "card_receipt": r}), "taskId", f.taskID, "requestId", id)
		req.Header.Set("X-User-ID", serviceID)
		return req.WithContext(middleware.SetMemberContext(req.Context(), testWorkspaceID, db.Member{}))
	}
	testutil.Call(t, testHandler.RespondTaskElicitation, request(nil)).Want(403)
	forged := receipt
	forged.RequestID = uuid.NewString()
	testutil.Call(t, testHandler.RespondTaskElicitation, request(&forged)).Want(403)
	machine := request(&receipt)
	machine.Header.Set("X-Actor-Source", "task_token")
	testutil.Call(t, testHandler.RespondTaskElicitation, machine).Want(403)
	testutil.Call(t, testHandler.RespondTaskElicitation, request(&receipt)).Want(200)
	testutil.Call(t, testHandler.RespondTaskElicitation, request(&receipt)).Want(409)
}

func TestTaskElicitationConcurrentConsumptionIsSingleUse(t *testing.T) {
	f, id := newApprovalFixture(t)
	testutil.Call(t, testHandler.RespondTaskElicitation, approvalHumanRequest(f.taskID, id, "once")).Want(200)
	results := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := testHandler.Queries.ConsumeTaskElicitation(t.Context(), db.ConsumeTaskElicitationParams{TaskID: parseUUID(f.taskID), WorkspaceID: parseUUID(testWorkspaceID), App: "fixture.app", OfferedScopes: []byte(`[]`)})
			results <- err
		}()
	}
	granted := 0
	for range 2 {
		if err := <-results; err == nil {
			granted++
		} else if !isNotFound(err) {
			t.Fatal(err)
		}
	}
	if granted != 1 {
		t.Fatalf("grants=%d", granted)
	}
}
