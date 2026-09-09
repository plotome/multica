package execenv

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	persistentLocalWorktreesDir         = ".persistent-local-worktrees"
	persistentLocalWorktreeStateFile    = "state.json"
	persistentLocalWorktreeStateVersion = 1
)

// PersistentLocalWorktreeRecord is the daemon-owned lifecycle record for one
// physical local_directory worktree. Its branch and worktree registration live
// in the user's repository; this record gives GC enough provenance to
// unregister the daemon-owned checkout without trusting paths discovered from
// the filesystem.
type PersistentLocalWorktreeRecord struct {
	Version          int       `json:"version"`
	WorkspaceID      string    `json:"workspace_id"`
	AgentID          string    `json:"agent_id"`
	ConversationID   string    `json:"conversation_id"`
	ConversationKind string    `json:"conversation_kind"`
	GitRoot          string    `json:"git_root"`
	WorktreePath     string    `json:"worktree_path"`
	WorkDir          string    `json:"work_dir"`
	Branch           string    `json:"branch"`
	Provider         string    `json:"provider"`
	LastUsedAt       time.Time `json:"last_used_at"`

	EntryRoot string `json:"-"`
}

// PersistentLocalWorktreesRoot returns the daemon-owned registry directory.
// It is deliberately outside task env roots: task logs, credentials and
// provider overlays keep their per-run lifecycle while only the checkout cwd
// survives between turns.
func PersistentLocalWorktreesRoot(workspacesRoot string) string {
	if strings.TrimSpace(workspacesRoot) == "" {
		return ""
	}
	return filepath.Join(workspacesRoot, persistentLocalWorktreesDir)
}

// PersistentLocalWorktreeEntryPath derives the stable lock / storage key for a
// (repository, agent, conversation) tuple. Names and issue identifiers are not
// part of identity because both can change; the canonical repository root and
// server UUIDs cannot.
func PersistentLocalWorktreeEntryPath(workspacesRoot, localPath, workspaceID, agentID, conversationKind, conversationID string) (string, error) {
	if strings.TrimSpace(workspacesRoot) == "" || workspaceID == "" || agentID == "" || conversationID == "" {
		return "", errors.New("execenv: persistent local worktree requires workspaces root, workspace, agent, and conversation ids")
	}
	if conversationKind != string(GCKindIssue) && conversationKind != string(GCKindChat) {
		return "", fmt.Errorf("execenv: unsupported persistent local worktree conversation kind %q", conversationKind)
	}
	gitRoot, err := resolveGitRoot(localPath)
	if err != nil {
		return "", err
	}
	identity := strings.Join([]string{workspaceID, gitRoot, agentID, conversationKind, conversationID}, "\x00")
	sum := sha256.Sum256([]byte(identity))
	return filepath.Join(PersistentLocalWorktreesRoot(workspacesRoot), hex.EncodeToString(sum[:16])), nil
}

func persistentLocalWorktreePath(entryRoot string) string {
	return filepath.Join(entryRoot, localWorktreeDirName)
}

func readPersistentLocalWorktreeRecord(entryRoot string) (PersistentLocalWorktreeRecord, error) {
	data, err := os.ReadFile(filepath.Join(entryRoot, persistentLocalWorktreeStateFile))
	if err != nil {
		return PersistentLocalWorktreeRecord{}, err
	}
	var record PersistentLocalWorktreeRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return PersistentLocalWorktreeRecord{}, fmt.Errorf("decode persistent local worktree state: %w", err)
	}
	record.EntryRoot = entryRoot
	if record.Version != persistentLocalWorktreeStateVersion || record.WorkspaceID == "" || record.AgentID == "" ||
		record.ConversationID == "" || record.GitRoot == "" || record.WorktreePath == "" || record.WorkDir == "" || record.Branch == "" || record.Provider == "" {
		return PersistentLocalWorktreeRecord{}, errors.New("persistent local worktree state is incomplete or has an unsupported version")
	}
	if record.ConversationKind != string(GCKindIssue) && record.ConversationKind != string(GCKindChat) {
		return PersistentLocalWorktreeRecord{}, fmt.Errorf("persistent local worktree has unsupported conversation kind %q", record.ConversationKind)
	}
	wantPath := persistentLocalWorktreePath(entryRoot)
	if filepath.Clean(record.WorktreePath) != filepath.Clean(wantPath) {
		return PersistentLocalWorktreeRecord{}, fmt.Errorf("persistent local worktree path %q does not match entry %q", record.WorktreePath, wantPath)
	}
	rel, err := filepath.Rel(record.WorktreePath, record.WorkDir)
	if err != nil || !filepath.IsLocal(rel) {
		return PersistentLocalWorktreeRecord{}, fmt.Errorf("persistent work dir %q is outside worktree %q", record.WorkDir, record.WorktreePath)
	}
	return record, nil
}

func writePersistentLocalWorktreeRecord(entryRoot string, record PersistentLocalWorktreeRecord) error {
	if err := os.MkdirAll(entryRoot, 0o700); err != nil {
		return fmt.Errorf("create persistent local worktree entry: %w", err)
	}
	record.Version = persistentLocalWorktreeStateVersion
	record.EntryRoot = ""
	record.LastUsedAt = time.Now().UTC()
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("encode persistent local worktree state: %w", err)
	}
	tmp, err := os.CreateTemp(entryRoot, ".state-*")
	if err != nil {
		return fmt.Errorf("create persistent local worktree state: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, filepath.Join(entryRoot, persistentLocalWorktreeStateFile)); err != nil {
		return fmt.Errorf("publish persistent local worktree state: %w", err)
	}
	return nil
}

func (w *LocalWorktree) persistRecord() error {
	if w == nil || !w.Persistent || w.PersistentEntryRoot == "" {
		return nil
	}
	return writePersistentLocalWorktreeRecord(w.PersistentEntryRoot, PersistentLocalWorktreeRecord{
		WorkspaceID:      w.owner.WorkspaceID,
		AgentID:          w.owner.AgentID,
		ConversationID:   w.owner.ConversationID,
		ConversationKind: w.PersistentConversationKind,
		GitRoot:          w.GitRoot,
		WorktreePath:     w.Path,
		WorkDir:          w.WorkDir,
		Branch:           w.Branch,
		Provider:         w.PersistentProvider,
	})
}

func cleanupPersistentLocalWorktreeArtifacts(entryRoot, workDir, provider string) error {
	var errs []error
	if err := CleanupRuntimeConfig(workDir, provider); err != nil {
		errs = append(errs, err)
	}
	if err := CleanupSidecars(entryRoot); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// ListPersistentLocalWorktrees returns only fully published, structurally
// valid records. Invalid entries are preserved for operator inspection; GC
// must never turn malformed provenance into permission to delete code.
func ListPersistentLocalWorktrees(workspacesRoot string, logger *slog.Logger) []PersistentLocalWorktreeRecord {
	root := PersistentLocalWorktreesRoot(workspacesRoot)
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	records := make([]PersistentLocalWorktreeRecord, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		entryRoot := filepath.Join(root, entry.Name())
		record, err := readPersistentLocalWorktreeRecord(entryRoot)
		if err != nil {
			if logger != nil {
				logger.Warn("execenv: skip invalid persistent local worktree record", "entry", entryRoot, "error", err)
			}
			continue
		}
		records = append(records, record)
	}
	return records
}

// RemovePersistentLocalWorktree checkpoints any recoverable edits, unregisters
// the linked worktree, and removes its daemon record. The branch is deliberately
// retained as the conversation's delivered artifact.
func RemovePersistentLocalWorktree(record PersistentLocalWorktreeRecord, logger *slog.Logger) error {
	current, err := readPersistentLocalWorktreeRecord(record.EntryRoot)
	if err != nil {
		return err
	}
	if current.WorkspaceID != record.WorkspaceID || current.AgentID != record.AgentID ||
		current.ConversationID != record.ConversationID || current.GitRoot != record.GitRoot || current.Branch != record.Branch {
		return errors.New("persistent local worktree identity changed before cleanup")
	}

	unlock, err := lockGitRoot(current.GitRoot, logger)
	if err != nil {
		return err
	}
	defer unlock()

	if _, err := os.Stat(current.WorktreePath); errors.Is(err, os.ErrNotExist) {
		return os.RemoveAll(current.EntryRoot)
	} else if err != nil {
		return err
	}
	if err := cleanupPersistentLocalWorktreeArtifacts(current.EntryRoot, current.WorkDir, current.Provider); err != nil {
		return fmt.Errorf("clean persistent local worktree runtime artifacts: %w", err)
	}
	branch, err := runGitTrimmed(current.WorktreePath, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || branch != current.Branch {
		return fmt.Errorf("persistent local worktree branch is %q, expected %q", branch, current.Branch)
	}
	if unmerged, err := unmergedPaths(current.WorktreePath); err != nil {
		return err
	} else if len(unmerged) > 0 {
		return fmt.Errorf("persistent local worktree has unresolved merges in %s", quotedPaths(unmerged))
	}
	if dirty, err := worktreeIsDirty(current.WorktreePath); err != nil {
		return err
	} else if dirty {
		if _, err := commitEverything(current.WorktreePath, "chore(agent): recovered changes before persistent worktree cleanup", false); err != nil {
			return fmt.Errorf("checkpoint persistent local worktree before cleanup: %w", err)
		}
	}
	if err := removeLocalWorktreeDir(current.GitRoot, current.WorktreePath, logger); err != nil {
		return err
	}
	return os.RemoveAll(current.EntryRoot)
}
