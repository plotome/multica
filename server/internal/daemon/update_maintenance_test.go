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
