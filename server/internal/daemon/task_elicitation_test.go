package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/multica-ai/multica/server/pkg/protocol"
	"net/http"
	"sync/atomic"
	"testing"
)

func TestTaskElicitationNeverPollsOrCancelsDeferredRecord(t *testing.T) {
	for _, mode := range []string{"pending", "grant", "old-server"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			d := taskSupplementTestDaemon(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != "POST" {
					t.Error("unexpected polling")
				}
				switch mode {
				case "pending":
					w.Write([]byte(`{"approval":{"code":"computer_use_approval_required","request_id":"approval","app":"fixture.app"}}`))
				case "grant":
					w.Write([]byte(`{"response":{"action":"accept","content":{}}}`))
				default:
					w.WriteHeader(404)
				}
			})
			response, err := d.requestTaskElicitation(context.Background(), "task", json.RawMessage(`{}`), d.logger)
			if calls.Load() != 1 {
				t.Fatalf("calls=%d", calls.Load())
			}
			if mode == "grant" {
				if err != nil || len(response) == 0 {
					t.Fatal(err, string(response))
				}
			} else if mode == "pending" {
				var required *protocol.ApplicationApprovalRequired
				if !errors.As(err, &required) || required.RequestID != "approval" {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("old server must fail closed")
			}
		})
	}
}
