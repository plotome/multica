package execenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPersistentLocalWorktreeRecoversMissingState(t *testing.T) {
	for _, partial := range []bool{true, false} {
		t.Run(map[bool]string{true: "interrupted-checkout", false: "preserve-edits"}[partial], func(t *testing.T) {
			repo := newTestRepo(t)
			params := persistentParamsForTest(t, repo, t.TempDir(), testBranchOwner.AgentID)
			entry, err := PersistentLocalWorktreeEntryPath(params.PersistentRoot, repo, params.WorkspaceID, params.AgentID, params.ConversationKind, params.ConversationID)
			if err != nil {
				t.Fatal(err)
			}
			path := persistentLocalWorktreePath(entry)
			if partial {
				gitRun(t, repo, "worktree", "add", "--no-checkout", "-b", "interrupted", path)
			} else {
				first, err := PrepareLocalWorktree(params, worktreeTestLogger())
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(filepath.Join(first.PersistentEntryRoot, persistentLocalWorktreeStateFile)); err != nil {
					t.Fatal(err)
				}
			}
			if partial {
				admin := gitRun(t, path, "rev-parse", "--absolute-git-dir")
				if err := os.WriteFile(filepath.Join(admin, "locked"), []byte("initializing"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(filepath.Join(path, "keep.txt"), []byte("saved edit"), 0600); err != nil {
					t.Fatal(err)
				}
				gitRun(t, path, "add", "keep.txt")
				gitRun(t, path, "commit", "-m", "saved commit")
				if err := os.WriteFile(filepath.Join(path, "untracked.txt"), []byte("saved untracked"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			savedTip := gitRun(t, path, "rev-parse", "HEAD")
			wt, err := PrepareLocalWorktree(params, worktreeTestLogger())
			if err != nil {
				t.Fatalf("retry after interrupted preparation: %v", err)
			}
			defer removePersistentForTest(t, wt)
			if got := readFile(t, filepath.Join(wt.Path, "tracked.txt")); got != "original\n" {
				t.Fatalf("retry checkout = %q", got)
			}
			archives, err := filepath.Glob(filepath.Join(filepath.Dir(entry), ".interrupted-"+filepath.Base(entry)+"-*", "entry", "worktree"))
			if err != nil || len(archives) != 1 {
				t.Fatalf("recovery archives = %v, %v", archives, err)
			}
			if got := gitRun(t, archives[0], "rev-parse", "HEAD"); got != savedTip {
				t.Fatalf("saved tip = %s, want %s", got, savedTip)
			}
			if !partial {
				if got := readFile(t, filepath.Join(archives[0], "untracked.txt")); got != "saved untracked" {
					t.Fatalf("lost edit: %q", got)
				}
				if _, err := wt.Finalize(worktreeTestLogger()); err != nil {
					t.Fatal(err)
				}
				next, err := PrepareLocalWorktree(params, worktreeTestLogger())
				if err != nil {
					t.Fatalf("follow-up after recovery: %v", err)
				}
				if next.Path != wt.Path || next.Branch != wt.Branch {
					t.Fatal("recovery did not remain reusable")
				}
			}
		})
	}
}

func TestPersistentLocalWorktreeDoesNotArchiveCorruptPublishedState(t *testing.T) {
	repo := newTestRepo(t)
	params := persistentParamsForTest(t, repo, t.TempDir(), testBranchOwner.AgentID)
	wt, err := PrepareLocalWorktree(params, worktreeTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(wt.PersistentEntryRoot, persistentLocalWorktreeStateFile)
	saved, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.WriteFile(statePath, saved, 0600); err != nil {
			t.Error(err)
			return
		}
		removePersistentForTest(t, wt)
	}()
	if err := os.WriteFile(statePath, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareLocalWorktree(params, worktreeTestLogger()); err == nil || !strings.Contains(err.Error(), "decode persistent local worktree state") {
		t.Fatalf("corrupt published state: %v", err)
	}
	if _, err := os.Stat(wt.Path); err != nil {
		t.Fatalf("corrupt checkout moved or removed: %v", err)
	}
}
