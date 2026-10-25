package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestApplicationApprovalCLIForwardsReceiptWithoutRetry(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile("receipt.json", []byte(`{"signature":"fixture"}`), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "POST" || r.URL.Path != "/api/tasks/task/elicitations/request/respond" {
			t.Error(r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["choice"] != "once" || body["card_receipt"].(map[string]any)["signature"] != "fixture" {
			t.Error(body)
		}
		w.WriteHeader(409)
		w.Write([]byte(`{"error":"already answered"}`))
	}))
	defer srv.Close()
	t.Setenv("MULTICA_SERVER_URL", srv.URL)
	t.Setenv("MULTICA_WORKSPACE_ID", "ws")
	t.Setenv("MULTICA_TOKEN", "fixture")
	cmd := newApplicationApprovalCommand()
	cmd.SetArgs([]string{"respond", "task", "request", "--choice", "once", "--card-receipt-file", "receipt.json"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("conflict must be exposed")
	}
	if calls != 1 {
		t.Fatal("ambiguous approval retried", calls)
	}
}
