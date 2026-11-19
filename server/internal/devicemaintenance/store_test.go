package devicemaintenance

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func fixture(t *testing.T) (*pgxpool.Pool, *testutil.Fixture, string, string) {
	t.Helper()
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(cfg.ConnConfig.Database, "multica_multica_") {
		t.Fatal("existing isolated worktree database required")
	}
	cfg.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = pool.Ping(ctx); err != nil {
		t.Fatal("isolated database unavailable")
	}
	t.Cleanup(pool.Close)
	boot := testutil.New(pool, "", "")
	id := uuid.NewString()
	owner := boot.User(t, "device maintenance", id+"@example.invalid")
	ws := boot.Workspace(t, "device maintenance", "device-"+id)
	fx := testutil.New(pool, ws, owner)
	fx.Member(t, ws, owner, "owner")
	device := uuid.NewString()
	rt := fx.Runtime(t, "fixture device", testutil.Cols{"daemon_id": device, "runtime_mode": "local", "provider": "codex"})
	t.Cleanup(func() {
		pool.Exec(ctx, `DELETE FROM device_maintenance WHERE daemon_id=$1`, device)
		pool.Exec(ctx, `DELETE FROM device_maintenance_event WHERE daemon_id=$1`, device)
	})
	return pool, fx, device, rt
}
func request() Request        { return Request{Lease: uuid.NewString(), Digest: strings.Repeat("d", 64)} }
func binding(s State) Request { return Request{s.Lease, s.Digest, s.Generation} }
func must(t *testing.T, s Store, device, actor, action string, r Request) State {
	t.Helper()
	v, e := s.Act(context.Background(), device, actor, action, r)
	if e != nil {
		t.Fatal(action, e)
	}
	return v
}
func task(t *testing.T, fx *testutil.Fixture, rt, status string) string {
	t.Helper()
	a := fx.Agent(t, "task agent "+uuid.NewString(), rt)
	i := fx.Issue(t, "device task")
	return fx.Task(t, a, testutil.Cols{"runtime_id": rt, "issue_id": i, "status": status})
}

func TestDeviceMaintenanceAtomicScopeAndRegistration(t *testing.T) {
	pool, fx, device, rt := fixture(t)
	s := Store{pool}
	ctx := context.Background()
	ws2 := fx.Workspace(t, "second scope", "device-scope-"+uuid.NewString())
	other := testutil.New(pool, ws2, fx.UserID)
	other.Runtime(t, "second provider", testutil.Cols{"daemon_id": device, "provider": "claude"})
	r := request()
	if _, e := s.Act(ctx, device, fx.UserID, "acquire", r); e == nil {
		t.Fatal("missing workspace membership admitted")
	}
	var count int
	if e := pool.QueryRow(ctx, `SELECT count(*) FROM device_maintenance WHERE daemon_id=$1`, device).Scan(&count); e != nil || count != 0 {
		t.Fatal("partial freeze", e, count)
	}
	other.Member(t, ws2, fx.UserID, "member")
	state := must(t, s, device, fx.UserID, "acquire", r)
	if !state.Frozen || len(state.Workspaces) != 2 || state.LocalDrained {
		t.Fatal(state)
	}
	if again := must(t, s, device, fx.UserID, "acquire", r); again.Generation != state.Generation {
		t.Fatal("acquire was not idempotent")
	}
	if _, e := s.Act(ctx, device, fx.UserID, "acquire", request()); e == nil {
		t.Fatal("conflicting lease admitted")
	}
	// A new provider on this device inherits the fence, not a copied status.
	newRT := fx.Runtime(t, "new provider", testutil.Cols{"daemon_id": device, "provider": "pi"})
	id := task(t, fx, newRT, "queued")
	if _, e := pool.Exec(ctx, `UPDATE agent_task_queue SET status='dispatched',dispatched_at=now() WHERE id=$1`, id); e == nil {
		t.Fatal("new registration bypassed freeze")
	}
	foreign := fx.User(t, "foreign", uuid.NewString()+"@example.invalid")
	_, e := pool.Exec(ctx, `INSERT INTO agent_runtime(workspace_id,daemon_id,name,runtime_mode,provider,owner_id,status)
        VALUES($1,$2,'forged','local','forged',$3,'online')`, fx.WorkspaceID, device, foreign)
	if e == nil {
		t.Fatal("same daemon ID with another authenticated owner was admitted")
	}
	if _, e = pool.Exec(ctx, `UPDATE agent_runtime SET status='online' WHERE id=$1`, rt); e != nil {
		t.Fatal("heartbeat blocked", e)
	}
	if !must(t, Store{pool}, device, fx.UserID, "status", Request{}).Frozen {
		t.Fatal("new backend instance lost freeze")
	}
	state = must(t, s, device, fx.UserID, "drain", binding(state))
	must(t, s, device, fx.UserID, "release", binding(state))
}

func TestDeviceMaintenanceLegacyWritesDrainAndRollback(t *testing.T) {
	pool, fx, device, rt := fixture(t)
	s := Store{pool}
	ctx := context.Background()
	active := task(t, fx, rt, "dispatched")
	queued := task(t, fx, rt, "queued")
	state := must(t, s, device, fx.UserID, "acquire", request())
	if state.Pending != 1 {
		t.Fatal("dispatched preparation omitted", state.Pending)
	}
	if _, e := s.Act(ctx, device, fx.UserID, "drain", binding(state)); e == nil {
		t.Fatal("preparing task treated as idle")
	}
	// SQL from an older backend remains fenced even if it has no lease API.
	if _, e := pool.Exec(ctx, `UPDATE agent_task_queue SET status='dispatched',dispatched_at=now() WHERE id=$1`, queued); e == nil {
		t.Fatal("legacy claim escaped database fence")
	}
	if _, e := pool.Exec(ctx, `UPDATE agent_task_queue SET dispatched_at=now() WHERE id=$1`, active); e == nil {
		t.Fatal("stale claim reclaim escaped fence")
	}
	if _, e := pool.Exec(ctx, `UPDATE agent_task_queue SET status='running' WHERE id=$1`, active); e != nil {
		t.Fatal("existing legacy start cannot drain", e)
	}
	if _, e := pool.Exec(ctx, `UPDATE agent_task_queue SET status='completed',completed_at=now(),result='{"process_cleanup_pending":true}' WHERE id=$1`, active); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Act(ctx, device, fx.UserID, "drain", binding(state)); e == nil {
		t.Fatal("terminal row confused with cleanup completion")
	}
	if _, e := pool.Exec(ctx, `UPDATE agent_task_queue SET result='{}' WHERE id=$1`, active); e != nil {
		t.Fatal(e)
	}
	state = must(t, s, device, fx.UserID, "drain", binding(state))
	if !state.ServerDrained || state.LocalDrained {
		t.Fatal("server proof misrepresented as local proof")
	}
	down, e := os.ReadFile(filepath.Join("../../migrations", "643_device_maintenance.down.sql"))
	if e != nil {
		t.Fatal(e)
	}
	tx, e := pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(ctx, string(down)); e == nil {
		t.Fatal("rollback removed a live database fence")
	}
	tx.Rollback(ctx)
	wrong := binding(state)
	wrong.Generation = uuid.NewString()
	if _, e = s.Act(ctx, device, fx.UserID, "release", wrong); e == nil {
		t.Fatal("wrong generation released")
	}
	if _, e = pool.Exec(ctx, `UPDATE agent_task_queue SET status='running' WHERE id=$1`, active); e == nil {
		t.Fatal("legacy start after server drain escaped")
	}
	released := must(t, s, device, fx.UserID, "release", binding(state))
	if released.Frozen {
		t.Fatal(released)
	}
	if _, e = pool.Exec(ctx, `UPDATE agent_task_queue SET status='dispatched',dispatched_at=now() WHERE id=$1`, queued); e != nil {
		t.Fatal("release did not restore admission", e)
	}
}

func TestDeviceMaintenanceAcquireWaitsForInFlightClaim(t *testing.T) {
	pool, fx, device, rt := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id := task(t, fx, rt, "queued")
	tx, e := pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(context.Background())
	if _, e = tx.Exec(ctx, `UPDATE agent_task_queue SET status='dispatched',dispatched_at=now() WHERE id=$1`, id); e != nil {
		t.Fatal(e)
	}
	var pid int
	if e = tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); e != nil {
		t.Fatal(e)
	}
	result := make(chan State, 1)
	failed := make(chan error, 1)
	go func() {
		state, err := (Store{pool}).Act(ctx, device, fx.UserID, "acquire", request())
		if err != nil {
			failed <- err
		} else {
			result <- state
		}
	}()
	// Observe the real PostgreSQL wait edge, rather than assuming a sleep
	// means the two transactions overlapped.
	for {
		var waiting bool
		if e = pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, pid).Scan(&waiting); e != nil {
			t.Fatal(e)
		}
		if waiting {
			break
		}
		select {
		case err := <-failed:
			t.Fatal(err)
		case <-result:
			t.Fatal("acquire passed in-flight claim")
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	select {
	case state := <-result:
		if state.Pending != 1 {
			t.Fatal("late delivered claim missing from drain", state)
		}
	case e := <-failed:
		t.Fatal(e)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestDeviceMaintenancePermissionRaceIsAtomic(t *testing.T) {
	pool, fx, device, _ := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, e := pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(context.Background())
	if _, e = tx.Exec(ctx, `DELETE FROM member WHERE workspace_id=$1 AND user_id=$2`, fx.WorkspaceID, fx.UserID); e != nil {
		t.Fatal(e)
	}
	var pid int
	if e = tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { _, e := (Store{pool}).Act(ctx, device, fx.UserID, "acquire", request()); done <- e }()
	for {
		var waiting bool
		if e = pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, pid).Scan(&waiting); e != nil {
			t.Fatal(e)
		}
		if waiting {
			break
		}
		select {
		case e := <-done:
			t.Fatal("acquire did not wait for membership mutation", e)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	if e = <-done; e == nil {
		t.Fatal("revoked membership froze device")
	}
	var count int
	if e = pool.QueryRow(ctx, `SELECT count(*) FROM device_maintenance WHERE daemon_id=$1`, device).Scan(&count); e != nil || count != 0 {
		t.Fatal("permission race left partial freeze", count, e)
	}
}

func TestDeviceMaintenanceFailedDeploymentDoesNotClaimCapability(t *testing.T) {
	pool, fx, device, _ := fixture(t)
	ctx := context.Background()
	state := must(t, Store{pool}, device, fx.UserID, "acquire", request())
	tx, e := pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, `SET LOCAL search_path = pg_catalog`); e != nil {
		t.Fatal(e)
	}
	if _, e = (Store{tx}).Act(ctx, device, fx.UserID, "capabilities", Request{}); e == nil {
		t.Fatal("backend without a migrated schema claimed readiness")
	}
	tx.Rollback(ctx)
	if !must(t, Store{pool}, device, fx.UserID, "status", Request{}).Frozen {
		t.Fatal("failed deployment released freeze")
	}
	state = must(t, Store{pool}, device, fx.UserID, "drain", binding(state))
	must(t, Store{pool}, device, fx.UserID, "release", binding(state))
}

func TestDeviceMaintenanceReleaseHistoryRespectsReownedIdentity(t *testing.T) {
	pool, fx, device, rt := fixture(t)
	s := Store{pool}
	ctx := context.Background()
	state := must(t, s, device, fx.UserID, "acquire", request())
	state = must(t, s, device, fx.UserID, "drain", binding(state))
	must(t, s, device, fx.UserID, "release", binding(state))
	if must(t, s, device, fx.UserID, "status", Request{}).Generation != state.Generation {
		t.Fatal("release reconciliation lost its audit binding")
	}
	other := fx.User(t, "new logical device owner", uuid.NewString()+"@example.invalid")
	fx.Member(t, fx.WorkspaceID, other, "member")
	if _, e := pool.Exec(ctx, `UPDATE agent_runtime SET owner_id=$2 WHERE id=$1`, rt, other); e != nil {
		t.Fatal(e)
	}
	// Restore the original owner before fixture FK cleanup.
	t.Cleanup(func() { pool.Exec(ctx, `UPDATE agent_runtime SET owner_id=$2 WHERE id=$1`, rt, fx.UserID) })
	observed := must(t, s, device, other, "status", Request{})
	if observed.Lease != "" || observed.Generation != "" || observed.Digest != "" {
		t.Fatal("another owner's release history leaked through reused daemon ID")
	}
}

func TestDeviceMaintenanceSurvivesHTTPServerAndPoolRestart(t *testing.T) {
	pool, fx, device, _ := fixture(t)
	ctx := context.Background()
	serve := func(db Database) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			(Store{db}).Serve(w, r, device, fx.UserID, strings.TrimPrefix(r.URL.Path, "/"))
		}))
	}
	call := func(server *httptest.Server, action string, binding *Request) State {
		t.Helper()
		var response *http.Response
		var err error
		if binding == nil {
			response, err = http.Get(server.URL + "/" + action)
		} else {
			raw, _ := json.Marshal(binding)
			response, err = http.Post(server.URL+"/"+action, "application/json", bytes.NewReader(raw))
		}
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatal(action, response.StatusCode)
		}
		var state State
		if err = json.NewDecoder(response.Body).Decode(&state); err != nil {
			t.Fatal(err)
		}
		return state
	}
	initial, err := pgxpool.NewWithConfig(ctx, pool.Config())
	if err != nil {
		t.Fatal(err)
	}
	defer initial.Close()
	before := serve(initial)
	defer before.Close()
	req := request()
	frozen := call(before, "acquire", &req)
	before.Close()
	initial.Close()
	fresh, err := pgxpool.NewWithConfig(ctx, pool.Config())
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	after := serve(fresh)
	defer after.Close()
	persisted := call(after, "status", nil)
	if !persisted.Frozen || persisted.Generation != frozen.Generation || persisted.Lease != frozen.Lease {
		t.Fatal("server/pool restart lost durable fence")
	}
	bound := binding(frozen)
	call(after, "drain", &bound)
	if call(after, "release", &bound).Frozen {
		t.Fatal("restarted server did not release the exact lease")
	}
}
