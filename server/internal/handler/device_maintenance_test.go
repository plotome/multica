package handler

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/daemonws"
	"github.com/multica-ai/multica/server/internal/devicemaintenance"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestDeviceMaintenanceRoutesFenceActualClaims(t *testing.T) {
	if testPool == nil {
		t.Fatal("real isolated database required")
	}
	ctx := context.Background()
	device := uuid.NewString()
	rt := dbfx.Runtime(t, "maintenance "+device, testutil.Cols{"daemon_id": device, "provider": "codex", "runtime_mode": "local"})
	agent := dbfx.Agent(t, "maintenance "+device, rt)
	issue := dbfx.Issue(t, "maintenance "+device)
	id := dbfx.Task(t, agent, testutil.Cols{"runtime_id": rt, "issue_id": issue})
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM device_maintenance WHERE daemon_id=$1`, device)
		testPool.Exec(ctx, `DELETE FROM device_maintenance_event WHERE daemon_id=$1`, device)
	})
	router := chi.NewRouter()
	router.With(RequireHumanActor).Post("/api/device-maintenance/{daemonId}/{action}", testHandler.DeviceMaintenance)
	router.With(RequireHumanActor).Get("/api/device-maintenance/{daemonId}/{action}", testHandler.DeviceMaintenance)
	call := func(action string, body any) *httptest.ResponseRecorder {
		req := newRequest("POST", "/api/device-maintenance/"+device+"/"+action, body)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	lease := devicemaintenance.Request{Lease: uuid.NewString(), Digest: strings.Repeat("a", 64)}
	machine := newRequest("POST", "/api/device-maintenance/"+device+"/acquire", lease)
	machine.Header.Set("X-Actor-Source", "task_token")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, machine)
	if w.Code != 403 {
		t.Fatal("task credential acquired maintenance", w.Code)
	}
	w = call("acquire", lease)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var state devicemaintenance.State
	if e := json.Unmarshal(w.Body.Bytes(), &state); e != nil {
		t.Fatal(e)
	}
	if !state.Frozen || !state.DatabaseFence || state.LocalDrained {
		t.Fatal(state)
	}
	assertQueued := func() {
		t.Helper()
		var status string
		if e := testPool.QueryRow(ctx, `SELECT status FROM agent_task_queue WHERE id=$1`, id).Scan(&status); e != nil || status != "queued" {
			t.Fatal("frozen route admitted task", e, status)
		}
	}
	single := newOwnerDaemonRequest("POST", "/api/daemon/runtimes/"+rt+"/tasks/claim", nil)
	single = withURLParam(single, "runtimeId", rt)
	w = httptest.NewRecorder()
	testHandler.ClaimTaskByRuntime(w, single)
	assertQueued()
	body := map[string]any{"daemon_id": device, "runtime_ids": []string{rt}, "max_tasks": 2}
	for _, path := range []string{"/api/daemon/tasks/claim", "/api/daemon/claim"} {
		w = httptest.NewRecorder()
		testHandler.ClaimTasksByRuntime(w, newOwnerDaemonRequest("POST", path, body))
		assertQueued()
	}
	raw, _ := json.Marshal(body)
	_, _, err := testHandler.DaemonRPCHandler(ctx, daemonws.ClientIdentity{UserID: testUserID, WorkspaceID: testWorkspaceID, RuntimeIDs: []string{rt}}, "tasks.claim", raw)
	if err != nil {
		t.Fatal(err)
	}
	assertQueued()
	// The generation from another transaction cannot drain or release this one.
	lease.Generation = uuid.NewString()
	if w = call("release", lease); w.Code != 409 {
		t.Fatal("foreign generation released", w.Code)
	}
	lease.Generation = state.Generation
	if w = call("drain", lease); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	// Legacy start is an actual handler call, including old empty payloads.
	for _, payload := range []any{map[string]any{}, map[string]any{"runtime_id": rt, "dispatched_at": "2000-01-01T00:00:00Z"}} {
		req := withURLParam(newOwnerDaemonRequest("POST", "/api/daemon/tasks/"+id+"/start", payload), "taskId", id)
		w = httptest.NewRecorder()
		testHandler.StartTask(w, req)
		assertQueued()
	}
	// A new handler object has no process-local frozen-state cache to lose.
	duplicate := *testHandler
	readRouter := chi.NewRouter()
	readRouter.Get("/api/device-maintenance/{daemonId}/{action}", duplicate.DeviceMaintenance)
	req := newRequest("GET", "/api/device-maintenance/"+device+"/status", nil)
	w = httptest.NewRecorder()
	readRouter.ServeHTTP(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"frozen":true`) {
		t.Fatal("backend restart lost device fence", w.Body.String())
	}
	if w = call("release", lease); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}
