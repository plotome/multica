package handler

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/forkupdate"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// This private host policy is never downloaded from a candidate feed.
type forkUpdatePolicy struct {
	Workspace   string                         `json:"workspace_id"`
	Owner       string                         `json:"owner_id"`
	ArtifactDir string                         `json:"artifact_dir,omitempty"`
	CatalogPath string                         `json:"catalog_path"`
	PublicKey   string                         `json:"public_key"`
	Origin      string                         `json:"origin"`
	Channel     string                         `json:"channel"`
	Hosts       map[string]string              `json:"host_runtime_ids"`
	Installed   map[string]forkupdate.Baseline `json:"installed"`
}

func (h *Handler) forkPolicy(w http.ResponseWriter, r *http.Request) (forkUpdatePolicy, bool) {
	var policy forkUpdatePolicy
	path := os.Getenv("MULTICA_FORK_UPDATE_CONFIG")
	info, err := os.Lstat(path)
	if !filepath.IsAbs(path) || err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 {
		writeError(w, http.StatusServiceUnavailable, "private Fork update channel is not configured")
		return policy, false
	}
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &policy) != nil || policy.Workspace == "" || policy.Owner == "" || !filepath.IsAbs(policy.CatalogPath) || len(policy.Hosts) == 0 || len(policy.Installed) == 0 {
		writeError(w, http.StatusServiceUnavailable, "invalid private Fork update policy")
		return policy, false
	}
	used := map[string]bool{}
	for host, runtimeID := range policy.Hosts {
		_, err := uuid.Parse(runtimeID)
		if host == "" || err != nil || used[runtimeID] {
			writeError(w, http.StatusServiceUnavailable, "invalid or duplicate update runtime binding")
			return policy, false
		}
		used[runtimeID] = true
	}
	return policy, true
}

func (h *Handler) forkState(r *http.Request, policy forkUpdatePolicy) (forkupdate.ControlState, error) {
	store := forkupdate.PostgresStore{DB: h.DB, Transactions: h.TxStarter}
	// Once approved, active recovery must not depend on a newer catalog file.
	// The persisted signed envelope is revalidated against the pinned trust key.
	persisted, loadErr := store.Latest(r.Context(), policy.Workspace)
	if loadErr != nil && !errors.Is(loadErr, pgx.ErrNoRows) {
		return persisted, loadErr
	}
	if loadErr == nil && persisted.Receipt.Approved {
		for _, target := range persisted.Receipt.Targets {
			if target.Phase != forkupdate.Succeeded && target.Phase != forkupdate.RolledBack {
				return h.verifyPersistedForkState(policy, persisted)
			}
		}
	}
	key, err := base64.StdEncoding.DecodeString(policy.PublicKey)
	if err != nil {
		return forkupdate.ControlState{}, err
	}
	data, err := os.ReadFile(policy.CatalogPath)
	if err != nil {
		return forkupdate.ControlState{}, err
	}
	candidate, err := forkupdate.VerifyCatalog(data, forkupdate.Policy{RequireSource: true, PublicKey: ed25519.PublicKey(key), Channel: policy.Channel, Origin: policy.Origin})
	if err != nil {
		return forkupdate.ControlState{}, err
	}
	state, err := store.Latest(r.Context(), policy.Workspace)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return state, err
	}
	if err == nil {
		if state.Receipt.Digest == candidate.Digest {
			return state, nil
		}
		if candidate.Sequence <= state.Receipt.Sequence {
			return state, errors.New("catalog sequence replay or downgrade")
		}
		for id, target := range state.Receipt.Targets {
			if state.Receipt.Approved && target.Phase != forkupdate.Succeeded && target.Phase != forkupdate.RolledBack {
				return state, nil
			}
			baseline := target.Baseline
			if target.Phase == forkupdate.Succeeded {
				baseline.Commit = target.Artifact.Commit
				baseline.Sequence = state.Receipt.Sequence
			}
			policy.Installed[id] = baseline
		}
	}
	receipt, err := forkupdate.Plan(candidate, policy.Installed)
	if err != nil {
		return state, err
	}
	for _, target := range receipt.Targets {
		if policy.Hosts[target.Artifact.Host] == "" {
			return state, errors.New("candidate host has no configured runtime")
		}
	}
	state = forkupdate.ControlState{Envelope: data, Receipt: receipt, Revision: 1}
	if err := store.Create(r.Context(), policy.Workspace, state); err != nil {
		if !errors.Is(err, forkupdate.ErrConflict) {
			return state, err
		}
	} else {
		h.forkChanged(policy, state)
	}
	return store.Latest(r.Context(), policy.Workspace)
}

func (h *Handler) forkOwner(w http.ResponseWriter, r *http.Request) (forkUpdatePolicy, bool) {
	policy, ok := h.forkPolicy(w, r)
	if !ok {
		return policy, false
	}
	if isMachineCredentialActor(r) || r.Header.Get("X-User-ID") != policy.Owner {
		writeError(w, http.StatusForbidden, "only the configured update owner may control this installation")
		return policy, false
	}
	_, ok = h.requireWorkspaceMember(w, r, policy.Workspace, "workspace not found")
	return policy, ok
}

func (h *Handler) GetForkUpdate(w http.ResponseWriter, r *http.Request) {
	policy, ok := h.forkOwner(w, r)
	if !ok {
		return
	}
	var state forkupdate.ControlState
	var err error
	if r.URL.Query().Get("catalog_refresh") == "0" {
		state, err = (forkupdate.PostgresStore{DB: h.DB, Transactions: h.TxStarter}).Latest(r.Context(), policy.Workspace)
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusOK, map[string]bool{"available": false})
			return
		}
		if err == nil {
			state, err = h.verifyPersistedForkState(policy, state)
		}
	} else {
		state, err = h.forkState(r, policy)
	}
	if err != nil {
		if os.IsNotExist(err) {
			_, loadErr := (forkupdate.PostgresStore{DB: h.DB, Transactions: h.TxStarter}).Latest(r.Context(), policy.Workspace)
			if errors.Is(loadErr, pgx.ErrNoRows) {
				writeJSON(w, http.StatusOK, map[string]bool{"available": false})
				return
			}
		}
		writeError(w, http.StatusServiceUnavailable, "Fork candidate could not be verified or loaded")
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (h *Handler) ActOnForkUpdate(w http.ResponseWriter, r *http.Request) {
	policy, ok := h.forkOwner(w, r)
	if !ok {
		return
	}
	var action struct {
		Action   string `json:"action"`
		Digest   string `json:"digest"`
		Target   string `json:"target"`
		Revision int64  `json:"revision"`
	}
	if json.NewDecoder(r.Body).Decode(&action) != nil {
		writeError(w, http.StatusBadRequest, "invalid update action")
		return
	}
	state, err := (forkupdate.PostgresStore{DB: h.DB, Transactions: h.TxStarter}).Latest(r.Context(), policy.Workspace)
	if err == nil {
		state, err = h.verifyPersistedForkState(policy, state)
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "update plan unavailable")
		return
	}
	if action.Revision != state.Revision {
		writeError(w, http.StatusConflict, forkupdate.ErrConflict.Error())
		return
	}
	if err := state.Action(action.Action, action.Digest, action.Target); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	h.saveForkState(w, r, policy, state)
}

func (h *Handler) forkConsumer(w http.ResponseWriter, r *http.Request) (forkUpdatePolicy, string, bool) {
	policy, ok := h.forkPolicy(w, r)
	if !ok {
		return policy, "", false
	}
	runtimeID := chi.URLParam(r, "runtimeId")
	runtime, ok := h.requireDaemonRuntimeAccess(w, r, runtimeID)
	if !ok {
		return policy, "", false
	}
	if uuidToString(runtime.WorkspaceID) != policy.Workspace {
		writeError(w, http.StatusNotFound, "update host not found")
		return policy, "", false
	}
	for host, id := range policy.Hosts {
		if id == uuidToString(runtime.ID) {
			return policy, host, true
		}
	}
	writeError(w, http.StatusNotFound, "update host not found")
	return policy, "", false
}

func (h *Handler) GetForkUpdateForConsumer(w http.ResponseWriter, r *http.Request) {
	policy, _, ok := h.forkConsumer(w, r)
	if !ok {
		return
	}
	var state forkupdate.ControlState
	var err error
	if r.URL.Query().Get("catalog_refresh") == "0" {
		state, err = (forkupdate.PostgresStore{DB: h.DB, Transactions: h.TxStarter}).Latest(r.Context(), policy.Workspace)
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusOK, forkupdate.ControlState{})
			return
		}
		if err == nil {
			state, err = h.verifyPersistedForkState(policy, state)
		}
	} else {
		state, err = h.forkState(r, policy)
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "update plan unavailable")
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (h *Handler) ReportForkUpdate(w http.ResponseWriter, r *http.Request) {
	policy, host, ok := h.forkConsumer(w, r)
	if !ok {
		return
	}
	var report struct {
		Target   string            `json:"target"`
		Result   forkupdate.Target `json:"result"`
		Revision int64             `json:"revision"`
	}
	if json.NewDecoder(r.Body).Decode(&report) != nil {
		writeError(w, http.StatusBadRequest, "invalid update report")
		return
	}
	state, err := (forkupdate.PostgresStore{DB: h.DB, Transactions: h.TxStarter}).Latest(r.Context(), policy.Workspace)
	if err == nil {
		state, err = h.verifyPersistedForkState(policy, state)
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "update plan unavailable")
		return
	}
	if report.Revision != state.Revision {
		writeError(w, http.StatusConflict, forkupdate.ErrConflict.Error())
		return
	}
	if err := state.ApplyReport(host, report.Target, report.Result); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	h.saveForkState(w, r, policy, state)
}

func (h *Handler) saveForkState(w http.ResponseWriter, r *http.Request, policy forkUpdatePolicy, state forkupdate.ControlState) {
	err := (forkupdate.PostgresStore{DB: h.DB, Transactions: h.TxStarter}).Save(r.Context(), policy.Workspace, state)
	if errors.Is(err, forkupdate.ErrConflict) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to persist update state")
		return
	}
	state.Revision++
	h.forkChanged(policy, state)
	writeJSON(w, http.StatusOK, state)
}

func (h *Handler) verifyPersistedForkState(policy forkUpdatePolicy, state forkupdate.ControlState) (forkupdate.ControlState, error) {
	key, err := base64.StdEncoding.DecodeString(policy.PublicKey)
	if err != nil {
		return state, err
	}
	catalog, err := forkupdate.VerifyCatalog(state.Envelope, forkupdate.Policy{RequireSource: true, PublicKey: ed25519.PublicKey(key), Channel: policy.Channel, Origin: policy.Origin})
	if err == nil && (catalog.Digest != state.Receipt.Digest || catalog.Sequence != state.Receipt.Sequence) {
		err = errors.New("persisted candidate identity differs")
	}
	return state, err
}

func (h *Handler) forkChanged(policy forkUpdatePolicy, state forkupdate.ControlState) {
	if h.Bus != nil {
		h.Bus.Publish(events.Event{Type: protocol.EventForkUpdateChanged, WorkspaceID: policy.Workspace, MemberPrivateUserID: policy.Owner, ActorType: "system", Payload: map[string]any{"digest": state.Receipt.Digest, "revision": state.Revision}})
	}
}

var forkAssetDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)

func (h *Handler) GetForkUpdateArtifact(w http.ResponseWriter, r *http.Request) {
	policy, host, ok := h.forkConsumer(w, r)
	if !ok {
		return
	}
	digest := chi.URLParam(r, "digest")
	if !forkAssetDigest.MatchString(digest) || !filepath.IsAbs(policy.ArtifactDir) {
		writeError(w, http.StatusNotFound, "update artifact not found")
		return
	}
	state, err := (forkupdate.PostgresStore{DB: h.DB, Transactions: h.TxStarter}).Latest(r.Context(), policy.Workspace)
	if err == nil {
		state, err = h.verifyPersistedForkState(policy, state)
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "update plan unavailable")
		return
	}
	var artifact *forkupdate.Artifact
	for _, target := range state.Receipt.Targets {
		if target.Artifact.Host == host && target.Artifact.SHA256 == digest {
			value := target.Artifact
			artifact = &value
			break
		}
	}
	if artifact == nil {
		writeError(w, http.StatusNotFound, "update artifact not found")
		return
	}
	path := filepath.Join(policy.ArtifactDir, digest)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() != artifact.Bytes {
		writeError(w, http.StatusServiceUnavailable, "immutable artifact unavailable")
		return
	}
	file, err := os.Open(path)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "immutable artifact unavailable")
		return
	}
	defer file.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("ETag", `"`+digest+`"`)
	http.ServeContent(w, r, digest, info.ModTime(), file)
}
