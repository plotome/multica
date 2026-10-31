package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestRunIssueCancelTaskByIssue(t *testing.T) {
	t.Chdir(t.TempDir())
	const issueID = "1881a167-4bb6-4602-944b-f40ce4192fe6"
	const runID = "aaaaaaaa-1111-4222-8333-444444444444"
	const otherRunID = "bbbbbbbb-1111-4222-8333-444444444444"
	for _, tc := range []struct {
		name, input, wantErr, wantCancel string
		active                           []string
	}{
		{name: "issue key cancels its only active run", input: "MUL-7", active: []string{runID}, wantCancel: runID},
		{name: "full issue UUID cancels its only active run", input: issueID, active: []string{runID}, wantCancel: runID},
		{name: "full run UUID still cancels that run", input: runID, wantCancel: runID},
		{name: "issue without active run", input: "MUL-7", wantErr: "no queued or running run"},
		{name: "issue with several active runs", input: "MUL-7", active: []string{runID, otherRunID}, wantErr: otherRunID},
		{name: "short run prefix still needs issue", input: "aaaaaaaa", wantErr: "require --issue"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cancelled []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/issues/MUL-7":
					_ = json.NewEncoder(w).Encode(map[string]any{"id": issueID, "identifier": "MUL-7"})
				case "/api/issues/" + issueID + "/active-task":
					tasks := []map[string]any{}
					for _, id := range tc.active {
						tasks = append(tasks, map[string]any{"id": id, "status": "running", "agent_id": "agent-1"})
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"tasks": tasks})
				case "/api/issues/" + runID + "/active-task":
					w.WriteHeader(http.StatusNotFound)
					_ = json.NewEncoder(w).Encode(map[string]any{"error": "issue not found"})
				case "/api/tasks/" + runID + "/cancel", "/api/tasks/" + otherRunID + "/cancel":
					if r.Method != http.MethodPost {
						t.Errorf("method = %s, want POST", r.Method)
					}
					id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/tasks/"), "/cancel")
					cancelled = append(cancelled, id)
					_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "status": "cancelled"})
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			t.Setenv("MULTICA_SERVER_URL", srv.URL)
			t.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
			t.Setenv("MULTICA_TOKEN", "test-token")
			cmd := &cobra.Command{Use: "cancel-task"}
			cmd.Flags().String("output", "json", "")
			cmd.Flags().String("issue", "", "")
			_, err := captureStdout(t, func() error { return runIssueCancelTask(cmd, []string{tc.input}) })
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || len(cancelled) != 0 {
					t.Fatalf("err=%v cancelled=%v, want %q and no mutation", err, cancelled, tc.wantErr)
				}
				return
			}
			if err != nil || len(cancelled) != 1 || cancelled[0] != tc.wantCancel {
				t.Fatalf("err=%v cancelled=%v, want %s", err, cancelled, tc.wantCancel)
			}
		})
	}
}

func TestRunIssueListOpenFiltersByUnfinishedCategory(t *testing.T) {
	queries, _ := newPropertyFilterTestServer(t)
	if err := listIssuesWithFlags(t, map[string][]string{"open": {"true"}}); err != nil {
		t.Fatalf("runIssueList --open: %v", err)
	}
	if len(*queries) != 1 {
		t.Fatalf("expected one /api/issues request, got %d", len(*queries))
	}
	q := (*queries)[0]
	if got := q.Get("status_category"); got != "unstarted,started" {
		t.Errorf("status_category = %q, want unstarted,started", got)
	}
	if q.Has("status") {
		t.Errorf("--open must not send a status filter, got %q", q.Get("status"))
	}

	err := listIssuesWithFlags(t, map[string][]string{"open": {"true"}, "status": {"todo"}})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") || len(*queries) != 1 {
		t.Fatalf("err=%v requests=%d, want mutual-exclusion error before any request", err, len(*queries))
	}
}
