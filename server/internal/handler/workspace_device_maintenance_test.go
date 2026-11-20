package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/devicemaintenance"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestDeleteWorkspace_PreservesDeviceFenceAndAuditAcrossWorkspaces(t *testing.T) {
	if testPool == nil {
		t.Fatal("real isolated database required")
	}
	ctx := context.Background()
	lockRollupSingleton(t)
	device, otherDevice := uuid.NewString(), uuid.NewString()
	victim := dbfx.Workspace(t, "maintenance deletion", "delete-maintenance-"+uuid.NewString())
	neighbor := dbfx.Workspace(t, "maintenance neighbor", "keep-maintenance-"+uuid.NewString())
	for _, ws := range []string{victim, neighbor} {
		dbfx.Member(t, ws, testUserID, "owner")
	}
	vfx := testutil.New(testPool, victim, testUserID)
	nfx := testutil.New(testPool, neighbor, testUserID)
	vfx.Runtime(t, "victim device", testutil.Cols{"daemon_id": device})
	nfx.Runtime(t, "shared device", testutil.Cols{"daemon_id": device})
	otherRuntime := nfx.Runtime(t, "another device", testutil.Cols{"daemon_id": otherDevice})
	agent := nfx.Agent(t, "fenced neighbor agent", otherRuntime)
	queued := nfx.Task(t, agent, testutil.Cols{"runtime_id": otherRuntime})
	for _, ws := range []string{victim, neighbor} {
		dbfx.Exec(t, `INSERT INTO fork_update_rollout(workspace_id,digest,sequence,body,revision) VALUES($1,$2,1,'{}',1)`, ws, strings.Repeat("a", 64))
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(ctx, `DELETE FROM device_maintenance WHERE daemon_id=ANY($1::text[])`, []string{device, otherDevice})
		_, _ = testPool.Exec(ctx, `DELETE FROM device_maintenance_event WHERE daemon_id=ANY($1::text[])`, []string{device, otherDevice})
		_, _ = testPool.Exec(ctx, `DELETE FROM fork_update_rollout WHERE workspace_id=ANY($1::uuid[])`, []string{victim, neighbor})
	})
	store := devicemaintenance.Store{DB: testPool}
	act := func(id, action string, r devicemaintenance.Request) devicemaintenance.State {
		t.Helper()
		s, err := store.Act(ctx, id, testUserID, action, r)
		if err != nil {
			t.Fatal(action, err)
		}
		return s
	}
	bind := func(s devicemaintenance.State) devicemaintenance.Request {
		return devicemaintenance.Request{Lease: s.Lease, Digest: s.Digest, Generation: s.Generation}
	}
	frozen := act(device, "acquire", devicemaintenance.Request{Lease: uuid.NewString(), Digest: strings.Repeat("b", 64)})
	otherFrozen := act(otherDevice, "acquire", devicemaintenance.Request{Lease: uuid.NewString(), Digest: strings.Repeat("c", 64)})
	initialAudit := map[string]int{}
	for _, id := range []string{device, otherDevice} {
		initialAudit[id] = dbfx.Count(t, `SELECT count(*) FROM device_maintenance_event WHERE daemon_id=$1`, id)
	}
	deleteVictim := func() *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		r := withURLParam(newRequest(http.MethodDelete, "/api/workspaces/"+victim, nil), "id", victim)
		testHandler.DeleteWorkspace(w, r)
		return w
	}
	if w := deleteVictim(); w.Code < 400 {
		t.Fatalf("frozen runtime deletion succeeded: %d", w.Code)
	}
	if got := dbfx.Count(t, `SELECT count(*) FROM workspace WHERE id=$1`, victim); got != 1 {
		t.Fatal("failed deletion did not roll back workspace")
	}
	if got := dbfx.Count(t, `SELECT count(*) FROM fork_update_rollout WHERE workspace_id=ANY($1::uuid[])`, []string{victim, neighbor}); got != 2 {
		t.Fatal("failed deletion lost rollout receipts")
	}
	for id, want := range initialAudit {
		if got := dbfx.Count(t, `SELECT count(*) FROM device_maintenance_event WHERE daemon_id=$1`, id); got != want || got == 0 {
			t.Fatal("failed deletion lost device audit", id, got, want)
		}
	}
	assertFrozen := func(id string, want devicemaintenance.State) {
		t.Helper()
		got := act(id, "status", devicemaintenance.Request{})
		gotScopes, wantScopes := slices.Clone(got.Workspaces), slices.Clone(want.Workspaces)
		slices.Sort(gotScopes)
		slices.Sort(wantScopes)
		if !got.Frozen || got.Generation != want.Generation || got.Lease != want.Lease || !slices.Equal(gotScopes, wantScopes) {
			t.Fatal("workspace deletion changed device freeze", got, want)
		}
	}
	assertFrozen(device, frozen)
	assertFrozen(otherDevice, otherFrozen)
	auditCount := dbfx.Count(t, `SELECT count(*) FROM device_maintenance_event WHERE daemon_id=$1`, otherDevice)
	frozen = act(device, "drain", bind(frozen))
	act(device, "release", bind(frozen))
	sharedAuditCount := dbfx.Count(t, `SELECT count(*) FROM device_maintenance_event WHERE daemon_id=$1`, device)
	if w := deleteVictim(); w.Code != http.StatusNoContent {
		t.Fatalf("released workspace deletion = %d: %s", w.Code, w.Body.String())
	}
	if got := dbfx.Count(t, `SELECT count(*) FROM fork_update_rollout WHERE workspace_id=$1`, victim); got != 0 {
		t.Fatal("workspace-owned rollout survived deletion")
	}
	if got := dbfx.Count(t, `SELECT count(*) FROM fork_update_rollout WHERE workspace_id=$1`, neighbor); got != 1 {
		t.Fatal("neighbor rollout was deleted")
	}
	if got := dbfx.Count(t, `SELECT count(*) FROM agent_runtime WHERE workspace_id=$1 AND daemon_id=$2`, neighbor, device); got != 1 {
		t.Fatal("shared device's neighbor runtime was deleted")
	}
	for id, want := range map[string]int{device: sharedAuditCount, otherDevice: auditCount} {
		if got := dbfx.Count(t, `SELECT count(*) FROM device_maintenance_event WHERE daemon_id=$1`, id); got != want || got == 0 {
			t.Fatal("device-level audit was deleted", id, got, want)
		}
	}
	assertFrozen(otherDevice, otherFrozen)
	if _, err := testPool.Exec(ctx, `UPDATE agent_task_queue SET status='dispatched',dispatched_at=now() WHERE id=$1`, queued); err == nil {
		t.Fatal("workspace deletion lifted another device's admission fence")
	}
	otherFrozen = act(otherDevice, "drain", bind(otherFrozen))
	act(otherDevice, "release", bind(otherFrozen))
}
