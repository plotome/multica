//go:build legacy_native

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/devicemaintenance"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Runs the installed bytes, not go-run or a rebuilt daemon. Only the provider
// executable and controlled transport failure points are fixtures.
func TestNativeOldDaemonExit(t *testing.T) {
	cases := []string{"idle", "late-claim", "prepare", "terminal-replay", "exit-interruption", "supervisor-inhibit"}
	if selected := os.Getenv("NATIVE_EXIT_CASES"); selected != "" {
		cases = strings.Split(selected, ",")
	}
	for _, scenario := range cases {
		t.Run(scenario, func(t *testing.T) { runNativeOldDaemonCase(t, scenario) })
	}
}

func runNativeOldDaemonCase(t *testing.T, scenario string) {
	if testPool == nil {
		t.Fatal("existing isolated database required")
	}
	var database string
	if e := testPool.QueryRow(context.Background(), `SELECT current_database()`).Scan(&database); e != nil || !strings.HasPrefix(database, "multica_multica_") {
		t.Fatal("isolated worktree database required")
	}
	binary, expected := os.Getenv("NATIVE_OLD_CLI"), os.Getenv("NATIVE_OLD_SHA256")
	python, runner := os.Getenv("NATIVE_EXIT_PYTHON"), os.Getenv("NATIVE_EXIT_RUNNER")
	if binary == "" || expected == "" || python == "" || runner == "" {
		t.Fatal("explicit installed binary/hash and runner required")
	}
	raw, e := os.ReadFile(binary)
	if e != nil {
		t.Fatal(e)
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != expected {
		t.Fatal("installed CLI bytes changed")
	}
	ctx := context.Background()
	boot := testutil.New(testPool, "", "")
	nonce := uuid.NewString()
	owner := boot.User(t, "native exit "+nonce, nonce+"@example.invalid")
	workspace := boot.Workspace(t, "native exit "+nonce, "native-exit-"+nonce)
	fx := testutil.New(testPool, workspace, owner)
	fx.Member(t, workspace, owner, "owner")
	token, e := auth.GeneratePATToken()
	if e != nil {
		t.Fatal(e)
	}
	pat, e := db.New(testPool).CreatePersonalAccessToken(ctx, db.CreatePersonalAccessTokenParams{
		UserID: util.MustParseUUID(owner), Name: "native-exit-fixture", TokenHash: auth.HashToken(token), TokenPrefix: token[:12],
	})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM personal_access_token WHERE user_id=$1`, owner) })
	_ = pat
	device := uuid.NewString()
	var mu sync.Mutex
	tasks := []string{}
	events := []string{}
	hold := ""
	released := make(chan struct{})
	closed := false
	unblock := func() {
		mu.Lock()
		defer mu.Unlock()
		if !closed {
			close(released)
			closed = true
		}
	}
	defer unblock()
	router := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/daemon/ws" {
			http.Error(w, "fixture selects native HTTP claim transport", 404)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/_native/") {
			if r.Header.Get("Authorization") != "Bearer "+token {
				http.Error(w, "test credential required", 403)
				return
			}
			action := strings.TrimPrefix(r.URL.Path, "/_native/")
			var body map[string]string
			if r.Method == "POST" {
				_ = json.NewDecoder(r.Body).Decode(&body)
			}
			switch action {
			case "hold":
				mu.Lock()
				hold = body["phase"]
				released = make(chan struct{})
				closed = false
				events = nil
				mu.Unlock()
				json.NewEncoder(w).Encode(map[string]bool{"ok": true})
			case "unblock":
				unblock()
				json.NewEncoder(w).Encode(map[string]bool{"ok": true})
			case "task":
				var runtime string
				err := testPool.QueryRow(ctx, `SELECT id::text FROM agent_runtime WHERE daemon_id=$1 AND workspace_id=$2 AND provider='claude'`, device, workspace).Scan(&runtime)
				if err != nil {
					http.Error(w, "native runtime not registered", 409)
					return
				}
				agent := fx.Agent(t, "native "+uuid.NewString(), runtime, testutil.Cols{"runtime_mode": "local", "model": "fixture", "instructions": "Synthetic fixture only; do not call tools."})
				issue := fx.Issue(t, "native "+uuid.NewString(), testutil.Cols{"status": "in_progress"})
				id := fx.Task(t, agent, testutil.Cols{"runtime_id": runtime, "issue_id": issue, "context": "{}"})
				mu.Lock()
				tasks = append(tasks, id)
				mu.Unlock()
				json.NewEncoder(w).Encode(map[string]string{"task": id})
			case "state":
				mu.Lock()
				ids := append([]string{}, tasks...)
				ev := append([]string{}, events...)
				mu.Unlock()
				states := []map[string]any{}
				for _, id := range ids {
					var status string
					var failure *string
					if err := testPool.QueryRow(ctx, `SELECT status,failure_reason FROM agent_task_queue WHERE id=$1`, id).Scan(&status, &failure); err != nil {
						http.Error(w, "task lookup failed", 500)
						return
					}
					states = append(states, map[string]any{"id": id, "status": status, "failure": failure})
				}
				json.NewEncoder(w).Encode(map[string]any{"tasks": states, "events": ev})
			default:
				http.NotFound(w, r)
			}
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/device-maintenance/") {
			parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
			if len(parts) != 4 || parts[2] != device || r.Header.Get("Authorization") != "Bearer "+token {
				http.Error(w, "fixture scope required", 403)
				return
			}
			(devicemaintenance.Store{DB: testPool}).Serve(w, r, device, owner, parts[3])
			return
		}
		mu.Lock()
		phase := hold
		gate := released
		mu.Unlock()
		target := ""
		if r.Method == "POST" && strings.Contains(r.URL.Path, "/tasks/") {
			if strings.HasSuffix(r.URL.Path, "/claim") || r.URL.Path == "/api/daemon/tasks/claim" {
				target = "claim"
			}
			if strings.HasSuffix(r.URL.Path, "/start") {
				target = "prepare"
			}
			if strings.HasSuffix(r.URL.Path, "/complete") {
				target = "terminal"
			}
		}
		if target != "" && target == phase {
			if phase == "terminal" || phase == "claim" {
				recorded := httptest.NewRecorder()
				testServer.Config.Handler.ServeHTTP(recorded, r)
				// Empty claim responses do not exercise a late dispatched claim.
				if phase == "claim" && !strings.Contains(recorded.Body.String(), `"id"`) {
					for k, v := range recorded.Header() {
						w.Header()[k] = v
					}
					w.WriteHeader(recorded.Code)
					w.Write(recorded.Body.Bytes())
					return
				}
				mu.Lock()
				events = append(events, phase+"-committed")
				mu.Unlock()
				select {
				case <-gate:
				case <-r.Context().Done():
					return
				}
				for k, v := range recorded.Header() {
					w.Header()[k] = v
				}
				w.WriteHeader(recorded.Code)
				w.Write(recorded.Body.Bytes())
				return
			}
			mu.Lock()
			events = append(events, "prepare-entered")
			mu.Unlock()
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
		}
		testServer.Config.Handler.ServeHTTP(w, r)
	})
	backend := httptest.NewServer(router)
	defer backend.Close()
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM device_maintenance WHERE daemon_id=$1`, device)
		testPool.Exec(ctx, `DELETE FROM device_maintenance_event WHERE daemon_id=$1`, device)
	})
	input, _ := json.Marshal(map[string]string{"backend": backend.URL, "token": token, "workspace": workspace, "device": device, "binary": binary, "sha256": expected, "scenario": scenario})
	cmd := exec.Command(python, runner)
	cmd.Stdin = strings.NewReader(string(input))
	output, e := cmd.CombinedOutput()
	// Never print test credentials/config; the runner emits only observations.
	if e != nil {
		t.Fatalf("native exit fixture: %v\n%s", e, output)
	}
	t.Log(string(output))
	after, e := os.ReadFile(binary)
	if e != nil {
		t.Fatal(e)
	}
	sum = sha256.Sum256(after)
	if hex.EncodeToString(sum[:]) != expected {
		t.Fatal("native binary changed during experiment")
	}
}
