package daemon

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestUpdateMaintenanceSurvivesKeepAliveRestart(t *testing.T) {
	cfg := Config{WorkspacesRoot: t.TempDir(), MaintenanceDir: t.TempDir(), CLICommit: strings.Repeat("a", 40)}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := New(cfg, logger)
	lease := updateMaintenanceRequest{updateLease: updateLease{ID: uuid.NewString(), Digest: strings.Repeat("b", 64)}, PID: os.Getpid(), Commit: cfg.CLICommit}
	call := func(d *Daemon, resume bool, req updateMaintenanceRequest) int {
		data, _ := json.Marshal(req)
		r := httptest.NewRecorder()
		d.updateMaintenanceHandler(resume).ServeHTTP(r, httptest.NewRequest("POST", "/updates/prepare", bytes.NewReader(data)))
		return r.Code
	}
	if code := call(d, false, lease); code != 200 {
		t.Fatalf("prepare: %d", code)
	}
	if d.tryEnterClaim() {
		t.Fatal("prepared daemon claimed task")
	}
	if code := call(d, false, lease); code != 200 {
		t.Fatalf("idempotent prepare: %d", code)
	}
	d = New(cfg, logger) // a KeepAlive relaunch of the same profile
	if d.tryEnterClaim() || d.trySetClaimBarrier() {
		t.Fatal("restarted daemon escaped durable maintenance")
	}
	d.releaseClaimBarrier()
	if d.tryEnterClaim() {
		t.Fatal("transient barrier release removed durable maintenance")
	}
	other := lease
	other.ID = uuid.NewString()
	if code := call(d, true, other); code != 409 {
		t.Fatalf("foreign resume: %d", code)
	}
	if code := call(d, false, other); code != 409 {
		t.Fatalf("foreign prepare: %d", code)
	}
	wrong := lease
	wrong.Commit = strings.Repeat("c", 40)
	if code := call(d, true, wrong); code != 409 {
		t.Fatalf("unverified identity resumed: %d", code)
	}
	if code := call(d, true, lease); code != 200 {
		t.Fatalf("resume: %d", code)
	}
	d = New(cfg, logger)
	if !d.tryEnterClaim() {
		t.Fatal("completed maintenance persisted")
	}
	d.exitClaim()
}

func TestBusyUpdateMaintenanceDoesNotPauseClaims(t *testing.T) {
	for _, kind := range []string{"task", "claim", "barrier"} {
		t.Run(kind, func(t *testing.T) {
			d := New(Config{WorkspacesRoot: t.TempDir(), MaintenanceDir: t.TempDir(), CLICommit: strings.Repeat("a", 40)}, slog.Default())
			switch kind {
			case "task":
				d.activeTasks.Store(1)
			case "claim":
				d.claimsInFlight = 1
			case "barrier":
				d.pauseClaims = true
			}
			req := updateMaintenanceRequest{updateLease: updateLease{ID: uuid.NewString(), Digest: strings.Repeat("b", 64)}, PID: os.Getpid(), Commit: d.cfg.CLICommit}
			data, _ := json.Marshal(req)
			r := httptest.NewRecorder()
			d.updateMaintenanceHandler(false).ServeHTTP(r, httptest.NewRequest("POST", "/updates/prepare", bytes.NewReader(data)))
			if r.Code != 409 || d.updateMaintenance {
				t.Fatalf("busy maintenance: %d, gate %v", r.Code, d.updateMaintenance)
			}
			if _, err := os.Stat(d.updateLeasePath()); !os.IsNotExist(err) {
				t.Fatal("busy prepare wrote a lease")
			}
		})
	}
}

func TestCorruptMaintenancePausesAndCannotBeOverwritten(t *testing.T) {
	cfg := Config{WorkspacesRoot: t.TempDir(), MaintenanceDir: t.TempDir(), CLICommit: strings.Repeat("a", 40)}
	d := New(cfg, slog.Default())
	if err := os.WriteFile(d.updateLeasePath(), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	d = New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if d.tryEnterClaim() {
		t.Fatal("corrupt durable lease allowed claim")
	}
	req := updateMaintenanceRequest{updateLease: updateLease{ID: uuid.NewString(), Digest: strings.Repeat("b", 64)}, PID: os.Getpid(), Commit: cfg.CLICommit}
	data, _ := json.Marshal(req)
	r := httptest.NewRecorder()
	d.updateMaintenanceHandler(false).ServeHTTP(r, httptest.NewRequest("POST", "/updates/prepare", bytes.NewReader(data)))
	if r.Code != 503 {
		t.Fatalf("corrupt lease overwritten: %d", r.Code)
	}
}

func TestUpdateStopRequiresExactDurableLease(t *testing.T) {
	d := New(Config{WorkspacesRoot: t.TempDir(), MaintenanceDir: t.TempDir(), CLICommit: strings.Repeat("a", 40)}, slog.Default())
	cancelled := make(chan struct{}, 1)
	d.cancelFunc = func() { cancelled <- struct{}{} }
	lease := updateMaintenanceRequest{updateLease: updateLease{ID: uuid.NewString(), Digest: strings.Repeat("b", 64)}, PID: os.Getpid(), Commit: d.cfg.CLICommit}
	invoke := func(stop bool, request updateMaintenanceRequest) int {
		data, _ := json.Marshal(request)
		out := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/updates/stop", bytes.NewReader(data))
		if stop {
			d.updateMaintenanceStopHandler().ServeHTTP(out, req)
		} else {
			d.updateMaintenanceHandler(false).ServeHTTP(out, req)
		}
		return out.Code
	}
	if invoke(true, lease) != 409 {
		t.Fatal("unprepared daemon stopped")
	}
	if invoke(false, lease) != 200 {
		t.Fatal("prepare failed")
	}
	for _, field := range []string{"lease", "pid", "commit"} {
		wrong := lease
		switch field {
		case "lease":
			wrong.ID = uuid.NewString()
		case "pid":
			wrong.PID++
		case "commit":
			wrong.Commit = strings.Repeat("c", 40)
		}
		if invoke(true, wrong) != 409 {
			t.Fatal("foreign stop accepted", field)
		}
	}
	select {
	case <-cancelled:
		t.Fatal("unauthorized cancellation")
	default:
	}
	if invoke(true, lease) != 200 {
		t.Fatal("verified stop rejected")
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("verified stop did not cancel")
	}
	if _, err := os.Stat(d.updateLeasePath()); err != nil {
		t.Fatal("stop lost durable gate", err)
	}
}
