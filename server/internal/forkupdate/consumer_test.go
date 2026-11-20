package forkupdate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type fixtureControl struct {
	mu       sync.Mutex
	state    ControlState
	offline  bool
	packages int
}

func fixtureConsumer(t *testing.T) (Consumer, *fixtureControl, *diskDriver) {
	t.Helper()
	control := &fixtureControl{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		control.mu.Lock()
		defer control.mu.Unlock()
		if control.offline {
			http.Error(w, "control restarting", http.StatusServiceUnavailable)
			return
		}
		if r.URL.Path == "/package" {
			control.packages++
			w.Write([]byte("package"))
			return
		}
		if r.Header.Get("Authorization") != "Bearer fixture-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(control.state)
			return
		}
		var report struct {
			Revision int64  `json:"revision"`
			Target   string `json:"target"`
			Result   Target `json:"result"`
		}
		if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if report.Revision != control.state.Revision {
			w.WriteHeader(http.StatusConflict)
			return
		}
		if err := control.state.ApplyReport("mini", report.Target, report.Result); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusConflict)
			return
		}
		control.state.Revision++
		json.NewEncoder(w).Encode(control.state)
	}))
	t.Cleanup(server.Close)
	catalog := catalogFixture()
	catalog.Artifacts = catalog.Artifacts[:1]
	catalog.Artifacts[0].URL = server.URL + "/package"
	envelope, policy := signFixture(t, catalog)
	policy.Origin = server.URL
	verified, err := VerifyCatalog(envelope, policy)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := Plan(verified, inventoryFixture(verified))
	if err != nil {
		t.Fatal(err)
	}
	control.state = ControlState{Envelope: envelope, Receipt: receipt, Revision: 1}
	driver := &diskDriver{root: t.TempDir(), probe: Probe{Online: true, RollbackAvailable: true}}
	consumer := Consumer{Client: ControlClient{BaseURL: server.URL, RuntimeID: "fixture-runtime", Token: "fixture-token", HTTP: server.Client()}, Policy: policy, Host: "mini", StateDir: t.TempDir(), LockDir: t.TempDir(), CacheDir: t.TempDir(), Artifacts: server.Client(), Driver: func(ctx context.Context, target Target, path string) (Driver, error) {
		if target.Operation == "install" && filepath.Base(path) != target.Artifact.SHA256 {
			t.Error("driver did not receive a verified immutable cache entry")
		}
		return driver, nil
	}}
	return consumer, control, driver
}

func TestConsumerAutomaticallyPreparesButWaitsForDurableApproval(t *testing.T) {
	c, control, d := fixtureConsumer(t)
	if err := c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d.installCalls != 0 {
		t.Fatal("preparation implicitly installed candidate")
	}
	control.mu.Lock()
	if control.packages != 1 || control.state.Receipt.Targets["mini-server"].Phase != Prepared {
		t.Fatal("candidate did not prepare through HTTP")
	}
	if err := control.state.Action("approve", control.state.Receipt.Digest, ""); err != nil {
		t.Fatal(err)
	}
	control.state.Revision++
	control.mu.Unlock()
	d.probe.Busy = true
	if err := c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d.installCalls != 0 {
		t.Fatal("busy device installed candidate")
	}
	d.probe.Busy = false
	if err := c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	control.mu.Lock()
	defer control.mu.Unlock()
	if d.installCalls != 1 || !control.state.Receipt.Complete() {
		t.Fatal("approved idle device was not verified")
	}
	if control.packages != 1 {
		t.Fatal("verified package redownloaded")
	}
}

func TestConsumerSurvivesControlServerRestartWithoutReinstall(t *testing.T) {
	c, control, d := fixtureConsumer(t)
	control.state.Receipt.Approved = true
	d.afterInstall = func() { control.mu.Lock(); control.offline = true; control.mu.Unlock() }
	if err := c.Tick(context.Background()); err == nil {
		t.Fatal("control disconnection was hidden")
	}
	journal := Journal{Path: filepath.Join(c.StateDir, control.state.Receipt.Digest+".json"), LockDir: c.LockDir}
	local, err := journal.Read()
	if err != nil {
		t.Fatal(err)
	}
	if local.Targets["mini-server"].Phase != Succeeded {
		t.Fatal("control outage interrupted local verification")
	}
	control.mu.Lock()
	control.offline = false
	control.mu.Unlock()
	c.Driver = func(context.Context, Target, string) (Driver, error) {
		t.Fatal("completed local installation was invoked again after restart")
		return nil, nil
	}
	if err := c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	control.mu.Lock()
	defer control.mu.Unlock()
	if d.installCalls != 1 || !control.state.Receipt.Complete() {
		t.Fatal("reconnection did not publish retained result")
	}
}

func TestConsumerCannotInstallAnAlteredControlPlan(t *testing.T) {
	c, control, d := fixtureConsumer(t)
	control.state.Receipt.Approved = true
	target := control.state.Receipt.Targets["mini-server"]
	target.Artifact.Commit = strings.Repeat("d", 40)
	control.state.Receipt.Targets["mini-server"] = target
	if err := c.Tick(context.Background()); err == nil || d.installCalls != 0 {
		t.Fatal("control server changed signed candidate")
	}
}

func TestConsumerRestartsDuringControlOutageAndFinishesOnlyInFlightWork(t *testing.T) {
	c, control, d := fixtureConsumer(t)
	// Prepare online without approval, then persist the approval and installation
	// checkpoint as a previous worker would before a process termination.
	if err := c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	control.state.Receipt.Approved = true
	if err := c.cacheControlState(control.state); err != nil {
		t.Fatal(err)
	}
	journal := Journal{Path: filepath.Join(c.StateDir, control.state.Receipt.Digest+".json"), LockDir: c.LockDir}
	if err := journal.withLock(func() error {
		r, err := journal.load()
		if err != nil {
			return err
		}
		r.Approved = true
		target := r.Targets["mini-server"]
		target.Phase = Verifying
		plan, _ := d.PrepareRecovery(context.Background(), target)
		target.Recovery = &Recovery{Plan: plan, Status: "prepared"}
		r.Targets["mini-server"] = target
		return journal.save(r)
	}); err != nil {
		t.Fatal(err)
	}
	target := control.state.Receipt.Targets["mini-server"]
	if err := d.Install(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	control.offline = true
	if err := c.Tick(context.Background()); err == nil {
		t.Fatal("control outage hidden")
	}
	r, err := journal.Read()
	if err != nil {
		t.Fatal(err)
	}
	if r.Targets["mini-server"].Phase != Succeeded || d.installCalls != 1 {
		t.Fatal("worker restart did not complete running verification without reinstall")
	}
	control.offline = false
	if err := c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !control.state.Receipt.Complete() {
		t.Fatal("recovered result not delivered")
	}
}

func TestCachedApprovalCannotStartOfflineInstallation(t *testing.T) {
	c, control, d := fixtureConsumer(t)
	if err := c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	control.state.Receipt.Approved = true
	if err := c.cacheControlState(control.state); err != nil {
		t.Fatal(err)
	}
	journal := Journal{Path: filepath.Join(c.StateDir, control.state.Receipt.Digest+".json"), LockDir: c.LockDir}
	if err := journal.Approve(control.state.Receipt.Digest); err != nil {
		t.Fatal(err)
	}
	control.offline = true
	if err := c.Tick(context.Background()); err == nil {
		t.Fatal("offline new operation accepted")
	}
	if d.installCalls != 0 {
		t.Fatal("cached approval started new install while disconnected")
	}
}

func TestConsumerRevalidatesPreparedCacheAtInstallBoundary(t *testing.T) {
	c, control, d := fixtureConsumer(t)
	if err := c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	target := control.state.Receipt.Targets["mini-server"]
	if err := os.WriteFile(filepath.Join(c.CacheDir, target.Artifact.SHA256), []byte("damaged"), 0600); err != nil {
		t.Fatal(err)
	}
	control.state.Receipt.Approved = true
	if err := c.Tick(context.Background()); err == nil {
		t.Fatal("corrupt prepared package installed")
	}
	if d.installCalls != 0 || d.rollbackCalls != 0 {
		t.Fatal("corrupt prepared bytes reached installation or baseline restoration")
	}
}
