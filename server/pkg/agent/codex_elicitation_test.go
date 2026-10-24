package agent

import (
	"context"
	"encoding/json"
	"github.com/multica-ai/multica/server/pkg/protocol"
	"strings"
	"testing"
)

const testApprovalRequest = `{"threadId":"thread","turnId":"turn","serverName":"cua_repl","mode":"form","message":"Allow access to Fixture?","requestedSchema":{"type":"object","properties":{}},"_meta":{"codex_approval_kind":"mcp_tool_call","connector_id":"computer-use","persist":["session"],"tool_params":{"app":"fixture.app"}}}`

func TestCodexElicitationHumanResponseAndCancellation(t *testing.T) {
	for _, end := range []string{"human", "interrupt", "resolved", "completed", "process"} {
		t.Run(end, func(t *testing.T) {
			c, fs, _ := newTestCodexClient(t)
			c.threadID = "thread"
			c.notificationProtocol = "raw"
			c.setActiveTurnID("turn")
			entered := make(chan struct{})
			answer := make(chan struct{})
			c.onElicitation = func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
				close(entered)
				select {
				case <-answer:
					return json.RawMessage(`{"action":"accept","content":{},"_meta":{"persist":"session"}}`), nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			c.handleElicitation(json.RawMessage(`"request"`), json.RawMessage(testApprovalRequest))
			<-entered
			if !c.hasPendingElicitation() || len(fs.Lines()) != 0 {
				t.Fatal("approval did not wait for a human")
			}
			// The read pump must remain available while the human has not answered.
			c.handleLine(`{"method":"thread/name/updated","params":{"threadId":"thread","threadName":"still responsive"}}`)
			switch end {
			case "human":
				close(answer)
			case "interrupt":
				c.cancelElicitations(true)
			case "resolved":
				c.resolveElicitation(map[string]any{"requestId": "request"})
			case "completed":
				c.handleLine(`{"method":"turn/completed","params":{"threadId":"thread","turn":{"id":"turn","status":"completed"}}}`)
			case "process":
				c.markProcessExited(nil)
			}
			c.elicitationWG.Wait()
			if c.hasPendingElicitation() {
				t.Fatal("approval retained after resolution")
			}
			lines := fs.Lines()
			if end == "resolved" || end == "process" {
				if len(lines) != 0 {
					t.Fatal(lines)
				}
				return
			}
			if len(lines) != 1 {
				t.Fatal(lines)
			}
			want := `"action":"cancel"`
			if end == "human" {
				want = `"action":"accept"`
			}
			if !strings.Contains(lines[0], want) || !strings.Contains(lines[0], `"id":"request"`) {
				t.Fatal(lines)
			}
		})
	}
}

func TestCodexElicitationCancelsThenDeliversStructuredResultToExactTurn(t *testing.T) {
	c, _, _ := newTestCodexClient(t)
	c.threadID = "thread"
	c.setActiveTurnID("turn")
	c.notificationProtocol = "raw"
	stdin := &fakeStdinWithHook{}
	stdin.afterWrite = func() { c.handleLine(`{"jsonrpc":"2.0","id":1,"result":{}}`) }
	c.stdin = stdin
	c.onElicitation = func(context.Context, json.RawMessage) (json.RawMessage, error) {
		return nil, &protocol.ApplicationApprovalRequired{Code: "computer_use_approval_required", RequestID: "approval", App: "fixture.app"}
	}
	c.handleElicitation(json.RawMessage(`"request"`), json.RawMessage(testApprovalRequest))
	c.elicitationWG.Wait()
	lines := stdin.Lines()
	if len(lines) != 2 || !strings.Contains(lines[0], `"action":"cancel"`) {
		t.Fatalf("not immediately cancelled: %v", lines)
	}
	var steer struct {
		Method string `json:"method"`
		Params struct {
			ThreadID string `json:"threadId"`
			TurnID   string `json:"expectedTurnId"`
			Input    []struct {
				Text string `json:"text"`
			} `json:"input"`
		} `json:"params"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &steer); err != nil {
		t.Fatal(err)
	}
	if steer.Method != "turn/steer" || steer.Params.ThreadID != "thread" || steer.Params.TurnID != "turn" || !strings.Contains(steer.Params.Input[0].Text, `"request_id":"approval"`) {
		t.Fatalf("lost structured result: %+v", steer)
	}
	if c.hasPendingElicitation() {
		t.Fatal("human wait survived")
	}
}
func TestCodexElicitationRejectsStaleTurnAndForgedResponse(t *testing.T) {
	c, fs, _ := newTestCodexClient(t)
	c.threadID = "thread"
	c.notificationProtocol = "raw"
	c.setActiveTurnID("different")
	c.onElicitation = func(context.Context, json.RawMessage) (json.RawMessage, error) {
		t.Error("stale approval reached human callback")
		return nil, nil
	}
	c.handleElicitation(json.RawMessage(`1`), json.RawMessage(testApprovalRequest))
	if len(fs.Lines()) != 1 || !strings.Contains(fs.Lines()[0], `"cancel"`) {
		t.Fatal(fs.Lines())
	}
	for _, response := range []string{`{"action":"accept","content":null}`, `{"action":"accept","content":{},"_meta":{"persist":"always"}}`, `{"action":"once"}`, `null`} {
		if validElicitationResponse(json.RawMessage(testApprovalRequest), json.RawMessage(response)) {
			t.Errorf("accepted %s", response)
		}
	}
}
