package handler

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/middleware"
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
	apiRouter := chi.NewRouter()
	apiServer := httptest.NewTLSServer(apiRouter)
	defer apiServer.Close()
	packageHash := fmt.Sprintf("%x", sha256.Sum256(packageBytes))
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	catalogPath := filepath.Join(dir, "catalog.json")
	configPath := filepath.Join(dir, "policy.json")
	artifact := forkupdate.Artifact{ID: "unit", Host: "mini", Kind: "cli-daemon", Platform: "darwin", Arch: "arm64", Version: "2.0", Commit: strings.Repeat("b", 40), URL: apiServer.URL + "/api/daemon/runtimes/" + testRuntimeID + "/fork-update/assets/" + packageHash, SHA256: packageHash, Bytes: int64(len(packageBytes)), ComponentVersions: map[string]string{"cli": "2.0", "daemon": "2.0"}}
	catalog := forkupdate.Catalog{Source: &forkupdate.SourceProof{Commit: strings.Repeat("b", 40), Repository: forkupdate.ForkSourceURL, GateSHA256: strings.Repeat("c", 64), Merged: true}, Schema: 1, Channel: "selfhost", Sequence: 2, Artifacts: []forkupdate.Artifact{artifact}}
	payload, _ := json.Marshal(catalog)
	envelope, _ := json.Marshal(forkupdate.Envelope{Payload: base64.StdEncoding.EncodeToString(payload), Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(private, payload))})
	artifactDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(artifactDir, packageHash), packageBytes, 0400); err != nil {
		t.Fatal(err)
	}
	policy := forkUpdatePolicy{ArtifactDir: artifactDir, Workspace: testWorkspaceID, Owner: testUserID, CatalogPath: catalogPath, PublicKey: base64.StdEncoding.EncodeToString(public), Origin: apiServer.URL, Channel: "selfhost", Hosts: map[string]string{"mini": testRuntimeID}, Installed: map[string]forkupdate.Baseline{"unit": {Host: "mini", Commit: strings.Repeat("a", 40), Sequence: 1, Platform: "darwin", Arch: "arm64"}}}
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
	// Real native authentication: fresh test-only PAT rows, Auth/DaemonAuth,
	// membership and runtime owner checks all run against the disposable DB.
	ownerPAT, err := auth.GeneratePATToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = testPool.Exec(context.Background(), `INSERT INTO personal_access_token(user_id,name,token_hash,token_prefix,expires_at) VALUES($1,'fork update isolation',$2,'mul_test',now()+interval '1 hour')`, testUserID, auth.HashToken(ownerPAT)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM personal_access_token WHERE token_hash=$1`, auth.HashToken(ownerPAT))
	})
	router := apiRouter
	router.Group(func(r chi.Router) {
		r.Use(middleware.Auth(restarted.Queries, nil, nil, nil))
		r.Get("/api/fork-updates", restarted.GetForkUpdate)
		r.With(RequireHumanActor).Post("/api/fork-updates/action", restarted.ActOnForkUpdate)
	})
	router.Group(func(r chi.Router) {
		r.Use(middleware.DaemonAuth(restarted.Queries, nil, nil, nil))
		r.Get("/api/daemon/runtimes/{runtimeId}/fork-update", restarted.GetForkUpdateForConsumer)
		r.Post("/api/daemon/runtimes/{runtimeId}/fork-update/report", restarted.ReportForkUpdate)
		r.Get("/api/daemon/runtimes/{runtimeId}/fork-update/assets/{digest}", restarted.GetForkUpdateArtifact)
	})
	artifactClient := *apiServer.Client()
	artifactClient.Transport = forkTestAuthTransport{base: artifactClient.Transport, token: ownerPAT}
	// Forged actor headers without a valid native credential cannot approve.
	actionBytes, _ := json.Marshal(map[string]any{"action": "approve", "digest": state.Receipt.Digest, "revision": state.Revision})
	for _, token := range []string{"invalid", ownerPAT} {
		req, _ := http.NewRequest(http.MethodPost, apiServer.URL+"/api/fork-updates/action", bytes.NewReader(actionBytes))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-User-ID", testUserID)
		response, err := apiServer.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		expected := http.StatusUnauthorized
		if token == ownerPAT {
			expected = http.StatusOK
		}
		if response.StatusCode != expected {
			t.Fatalf("native owner authentication status %d, expected %d", response.StatusCode, expected)
		}
	}
	driver := &forkHandlerDriver{path: filepath.Join(t.TempDir(), "installed"), busy: true}
	consumer := forkupdate.Consumer{Client: forkupdate.ControlClient{BaseURL: apiServer.URL, RuntimeID: testRuntimeID, Token: ownerPAT, HTTP: apiServer.Client()}, Policy: forkupdate.Policy{PublicKey: public, Origin: apiServer.URL, Channel: "selfhost"}, Host: "mini", StateDir: t.TempDir(), LockDir: t.TempDir(), CacheDir: t.TempDir(), Artifacts: &artifactClient, Driver: func(context.Context, forkupdate.Target, string) (forkupdate.Driver, error) { return driver, nil }}
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
	return forkupdate.Observation{At: time.Now().UTC().Format(time.RFC3339Nano), Components: []forkupdate.InstalledComponent{{Kind: "cli", Version: "2.0", Commit: commit}, {Kind: "daemon", Version: "2.0", Commit: commit}}}, nil
}

func (d *forkHandlerDriver) PrepareRecovery(_ context.Context, target forkupdate.Target) (forkupdate.RecoveryPlan, error) {
	observation := forkupdate.Observation{At: time.Now().UTC().Format(time.RFC3339Nano)}
	for _, kind := range forkupdate.ExpectedComponents(target.Artifact.Kind) {
		observation.Components = append(observation.Components, forkupdate.InstalledComponent{Kind: kind, Version: "fixture baseline", Commit: target.Baseline.Commit})
	}
	return forkupdate.RecoveryPlan{CandidateCommit: target.Artifact.Commit, BaselineCommit: target.Baseline.Commit, AssetsSHA256: strings.Repeat("c", 64), CompatibilitySHA256: strings.Repeat("d", 64), ForwardCompatible: true, BackwardCompatible: true, Baseline: observation}, nil
}

type forkTestAuthTransport struct {
	base  http.RoundTripper
	token string
}

func (t forkTestAuthTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.Header = r.Header.Clone()
	clone.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(clone)
}
