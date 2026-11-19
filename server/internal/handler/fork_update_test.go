package handler

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/go-chi/chi/v5"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/forkupdate"
)

func TestForkUpdateOwnerApprovalPersistsAndMachineActorsCannotApprove(t *testing.T) {
	if testHandler == nil {
		t.Skip("disposable database required")
	}
	packageBytes := []byte("fixture package")
	artifactServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(packageBytes) }))
	defer artifactServer.Close()
	packageHash := fmt.Sprintf("%x", sha256.Sum256(packageBytes))
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	catalogPath := filepath.Join(dir, "catalog.json")
	configPath := filepath.Join(dir, "policy.json")
	artifact := forkupdate.Artifact{ID: "unit", Host: "mini", Kind: "cli-daemon", Platform: "darwin", Arch: "arm64", Version: "2.0", Commit: strings.Repeat("b", 40), URL: artifactServer.URL + "/package", SHA256: packageHash, Bytes: int64(len(packageBytes)), ComponentVersions: map[string]string{"cli": "2.0", "daemon": "2.0"}}
	catalog := forkupdate.Catalog{Schema: 1, Channel: "selfhost", Sequence: 2, Artifacts: []forkupdate.Artifact{artifact}}
	payload, _ := json.Marshal(catalog)
	envelope, _ := json.Marshal(forkupdate.Envelope{Payload: base64.StdEncoding.EncodeToString(payload), Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(private, payload))})
	policy := forkUpdatePolicy{Workspace: testWorkspaceID, Owner: testUserID, CatalogPath: catalogPath, PublicKey: base64.StdEncoding.EncodeToString(public), Origin: artifactServer.URL, Channel: "selfhost", Hosts: map[string]string{"mini": testRuntimeID}, Installed: map[string]forkupdate.Baseline{"unit": {Host: "mini", Commit: strings.Repeat("a", 40), Sequence: 1, Platform: "darwin", Arch: "arm64"}}}
	data, _ := json.Marshal(policy)
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(catalogPath, envelope, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MULTICA_FORK_UPDATE_CONFIG", configPath)
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM fork_update_rollout WHERE workspace_id=$1`, testWorkspaceID)
	})
	get := httptest.NewRecorder()
	testHandler.GetForkUpdate(get, newRequestAs(testUserID, http.MethodGet, "/api/fork-updates", nil))
	if get.Code != http.StatusOK {
		t.Fatalf("get %d: %s", get.Code, get.Body.String())
	}
	var state forkupdate.ControlState
	if err := json.Unmarshal(get.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	action := map[string]any{"action": "approve", "digest": state.Receipt.Digest, "revision": state.Revision}
	for _, actor := range []string{"machine", "outsider"} {
		req := newRequestAs(testUserID, http.MethodPost, "/api/fork-updates/action", action)
		if actor == "machine" {
			req.Header.Set("X-Actor-Source", "task_token")
		} else {
			req.Header.Set("X-User-ID", "00000000-0000-0000-0000-000000000001")
		}
		rec := httptest.NewRecorder()
		testHandler.ActOnForkUpdate(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s authorized: %d %s", actor, rec.Code, rec.Body.String())
		}
	}
	approve := httptest.NewRecorder()
	testHandler.ActOnForkUpdate(approve, newRequestAs(testUserID, http.MethodPost, "/api/fork-updates/action", action))
	if approve.Code != http.StatusOK {
		t.Fatalf("approve %d: %s", approve.Code, approve.Body.String())
	}
	restarted := *testHandler
	get = httptest.NewRecorder()
	restarted.GetForkUpdate(get, newRequestAs(testUserID, http.MethodGet, "/api/fork-updates", nil))
	if err := json.Unmarshal(get.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if !state.Receipt.Approved || state.Revision != 2 {
		t.Fatal("restart lost durable approval")
	}
	stale := httptest.NewRecorder()
	testHandler.ActOnForkUpdate(stale, newRequestAs(testUserID, http.MethodPost, "/api/fork-updates/action", action))
	if stale.Code != http.StatusConflict {
		t.Fatal("stale approval overwrote state")
	}
	// Exercise the real consumer over HTTP against these handlers and PostgreSQL.
	// Authentication is explicitly a fixture; middleware credentials are not
	// installed on a live runtime or copied into a preview.
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer fixture" {
				w.WriteHeader(401)
				return
			}
			r.Header.Set("X-User-ID", testUserID)
			next.ServeHTTP(w, r)
		})
	})
	router.Get("/api/daemon/runtimes/{runtimeId}/fork-update", restarted.GetForkUpdateForConsumer)
	router.Post("/api/daemon/runtimes/{runtimeId}/fork-update/report", restarted.ReportForkUpdate)
	apiServer := httptest.NewServer(router)
	defer apiServer.Close()
	driver := &forkHandlerDriver{path: filepath.Join(t.TempDir(), "installed"), busy: true}
	consumer := forkupdate.Consumer{Client: forkupdate.ControlClient{BaseURL: apiServer.URL, RuntimeID: testRuntimeID, Token: "fixture", HTTP: apiServer.Client()}, Policy: forkupdate.Policy{PublicKey: public, Origin: artifactServer.URL, Channel: "selfhost"}, Host: "mini", StateDir: t.TempDir(), LockDir: t.TempDir(), CacheDir: t.TempDir(), Artifacts: artifactServer.Client(), Driver: func(context.Context, forkupdate.Target, string) (forkupdate.Driver, error) { return driver, nil }}
	if err := consumer.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	store := forkupdate.PostgresStore{DB: testPool, Transactions: testPool}
	state, err = store.Latest(context.Background(), testWorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Receipt.Targets["unit"].Phase != forkupdate.Waiting || driver.installs != 0 {
		t.Fatal("busy HTTP consumer installed")
	}
	// A broken newly published catalog must not strand an already approved
	// operation or reject its retained result.
	if err := os.WriteFile(catalogPath, []byte("corrupt incoming catalog"), 0600); err != nil {
		t.Fatal(err)
	}
	driver.busy = false
	if err := consumer.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err = store.Latest(context.Background(), testWorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Receipt.Complete() || driver.installs != 1 {
		t.Fatal("HTTP consumer result was not persisted")
	}
	// After the operation completes, an invalid incoming catalog is visible;
	// the already successful installation must not run again.
	if err := consumer.Tick(context.Background()); err == nil {
		t.Fatal("invalid incoming catalog was hidden")
	}
	if driver.installs != 1 {
		t.Fatal("catalog failure repeated completed install")
	}
	if err := os.WriteFile(catalogPath, envelope, 0600); err != nil {
		t.Fatal(err)
	}
	// A recreated consumer reconciles the persisted receipt without another install.
	if err := consumer.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if driver.installs != 1 {
		t.Fatal("completed install repeated")
	}
}

type forkHandlerDriver struct {
	path     string
	busy     bool
	installs int
}

func (d *forkHandlerDriver) Probe(context.Context, forkupdate.Target) (forkupdate.Probe, error) {
	return forkupdate.Probe{Online: true, Busy: d.busy, RollbackAvailable: true}, nil
}
func (d *forkHandlerDriver) Install(_ context.Context, target forkupdate.Target) error {
	d.installs++
	return os.WriteFile(d.path, []byte(target.Artifact.Commit), 0600)
}
func (d *forkHandlerDriver) Verify(_ context.Context, target forkupdate.Target) (forkupdate.Observation, error) {
	return d.observe(target.Artifact.Commit)
}
func (d *forkHandlerDriver) Rollback(_ context.Context, target forkupdate.Target) error {
	return os.WriteFile(d.path, []byte(target.Baseline.Commit), 0600)
}
func (d *forkHandlerDriver) VerifyRollback(_ context.Context, target forkupdate.Target) (forkupdate.Observation, error) {
	return d.observe(target.Baseline.Commit)
}
func (d *forkHandlerDriver) observe(commit string) (forkupdate.Observation, error) {
	data, err := os.ReadFile(d.path)
	if err != nil {
		return forkupdate.Observation{}, err
	}
	if string(data) != commit {
		return forkupdate.Observation{}, fmt.Errorf("running fixture revision differs")
	}
	return forkupdate.Observation{At: time.Now().UTC().Format(time.RFC3339Nano), Components: []forkupdate.InstalledComponent{{Kind: "cli", Version: "fixture", Commit: commit}, {Kind: "daemon", Version: "fixture", Commit: commit}}}, nil
}
