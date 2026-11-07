package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestIssueRunDiagnosticsResolvesScopedRunAndRejectsUnsupportedResponses(t *testing.T) {
	t.Chdir(t.TempDir())
	const issueID = "12345678-1234-4234-8234-123456789abc"
	const taskID = "abcdef12-1234-4234-8234-123456789abc"
	valid := `{"version":1,"task_id":"` + taskID + `","status":"failed","model_activity":"unknown","persistence":"unknown","messages":[],"session_id":null,"session_rollout_missing":false}`
	for _, tc := range []struct {
		name, body, output string
		status             int
		wantError          bool
	}{
		{"json", valid, "json", 200, false},
		{"table", valid, "table", 200, false},
		{"old-server", `{}`, "json", 200, true},
		{"missing-rollout-marker", strings.Replace(valid, `,"session_rollout_missing":false`, ``, 1), "json", 200, true},
		{"null-rollout-marker", strings.Replace(valid, `"session_rollout_missing":false`, `"session_rollout_missing":null`, 1), "json", 200, true},
		{"wrong-task", strings.Replace(valid, taskID, issueID, 1), "json", 200, true},
		{"malformed", `{"version":"one"}`, "json", 200, true},
		{"absent-endpoint", `{"error":"not found"}`, "json", 404, true},
		{"unknown-stage", strings.Replace(valid, `"model_activity":"unknown"`, `"model_activity":"not_started"`, 1), "json", 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			diagnosticsRead := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("unexpected write: %s", r.Method)
				}
				switch r.URL.Path {
				case "/api/issues/MUL-1852":
					json.NewEncoder(w).Encode(map[string]any{"id": issueID, "identifier": "MUL-1852"})
				case "/api/issues/" + issueID + "/task-runs":
					json.NewEncoder(w).Encode([]map[string]any{{"id": taskID}})
				case "/api/tasks/" + taskID + "/diagnostics":
					diagnosticsRead = true
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tc.status)
					w.Write([]byte(tc.body))
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			t.Setenv("MULTICA_SERVER_URL", srv.URL)
			t.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
			t.Setenv("MULTICA_TOKEN", "test-token")
			cmd := &cobra.Command{Use: "run-diagnostics"}
			cmd.Flags().String("output", tc.output, "")
			cmd.Flags().String("issue", "MUL-1852", "")
			out, err := captureStdout(t, func() error { return runIssueRunDiagnostics(cmd, []string{"abcd"}) })
			if (err != nil) != tc.wantError {
				t.Fatalf("error=%v, wantError=%v", err, tc.wantError)
			}
			if !diagnosticsRead {
				t.Fatal("single-task diagnostic endpoint was not read")
			}
			if tc.wantError && out != "" {
				t.Fatalf("error must not emit misleading evidence: %q", out)
			}
			if !tc.wantError && !strings.Contains(out, "unknown") {
				t.Fatalf("missing explicit unknown: %q", out)
			}
			if !tc.wantError && tc.output == "json" {
				var got map[string]any
				if err := json.Unmarshal([]byte(out), &got); err != nil {
					t.Fatal(err)
				}
				if got["session_id"] != nil {
					t.Fatalf("null session pointer changed: %v", got)
				}
			}
		})
	}
}
