//go:build !windows

package execenv

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// killWorktreeAdd puts a git on PATH whose `worktree add` is SIGKILLed after it
// registered the worktree and created the branch, the way an expired gitTimeout
// kills it mid-checkout: git never reaches its own cleanup, so the registration
// keeps the "initializing" lock.
func killWorktreeAdd(t *testing.T) (restore func()) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
if [ "$3" = worktree ] && [ "$4" = add ]; then
  root="$2"
  shift 4
  '%[1]s' -C "$root" worktree add --lock --reason initializing --no-checkout "$@" || exit $?
  kill -9 $$
fi
exec '%[1]s' "$@"
`, realGit)
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	path := os.Getenv("PATH")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+path)
	return func() { t.Setenv("PATH", path) }
}

func TestLocalWorktreeRetriesAfterKilledWorktreeAdd(t *testing.T) {
	for _, persistent := range []bool{true, false} {
		t.Run(map[bool]string{true: "persistent", false: "per-task"}[persistent], func(t *testing.T) {
			repo := newTestRepo(t)
			params := persistentParamsForTest(t, repo, t.TempDir(), testBranchOwner.AgentID)
			if !persistent {
				params.PersistentRoot = ""
			}
			before := gitRun(t, repo, "for-each-ref", "--format=%(refname)", "refs/heads")

			restore := killWorktreeAdd(t)
			if _, err := PrepareLocalWorktree(params, worktreeTestLogger()); err == nil || !strings.Contains(err.Error(), "killed") {
				t.Fatalf("killed worktree add: err = %v", err)
			}
			restore()

			if list := gitRun(t, repo, "worktree", "list", "--porcelain"); strings.Contains(list, "locked") || strings.Count(list, "worktree ") != 1 {
				t.Fatalf("killed worktree add left a registration behind:\n%s", list)
			}
			if after := gitRun(t, repo, "for-each-ref", "--format=%(refname)", "refs/heads"); after != before {
				t.Fatalf("killed worktree add left a branch behind:\n%s", after)
			}
			wt, err := PrepareLocalWorktree(params, worktreeTestLogger())
			if err != nil {
				t.Fatalf("retry after killed worktree add: %v", err)
			}
			if persistent {
				defer removePersistentForTest(t, wt)
			}
			if got := readFile(t, filepath.Join(wt.Path, "tracked.txt")); got != "original\n" {
				t.Fatalf("retry checkout = %q", got)
			}
		})
	}
}

// A registration left locked by a daemon that predates the rollback above (or
// that was itself killed before rolling back) must not fail every later run.
func TestPersistentLocalWorktreeClearsMissingInitializingRegistration(t *testing.T) {
	for reason, recovers := range map[string]bool{"initializing": true, "kept by user": false} {
		t.Run(strings.ReplaceAll(reason, " ", "-"), func(t *testing.T) {
			repo := newTestRepo(t)
			params := persistentParamsForTest(t, repo, t.TempDir(), testBranchOwner.AgentID)
			entry, err := PersistentLocalWorktreeEntryPath(params.PersistentRoot, repo, params.WorkspaceID, params.AgentID, params.ConversationKind, params.ConversationID)
			if err != nil {
				t.Fatal(err)
			}
			path := persistentLocalWorktreePath(entry)
			gitRun(t, repo, "worktree", "add", "--lock", "--reason", reason, "-b", "left-behind", path)
			if err := os.RemoveAll(entry); err != nil {
				t.Fatal(err)
			}

			wt, err := PrepareLocalWorktree(params, worktreeTestLogger())
			if !recovers {
				if err == nil || !strings.Contains(err.Error(), "missing but locked") {
					t.Fatalf("a lock git did not set itself must be respected: %v", err)
				}
				if list := gitRun(t, repo, "worktree", "list", "--porcelain"); !strings.Contains(list, "locked "+reason) {
					t.Fatalf("user lock was dropped:\n%s", list)
				}
				return
			}
			if err != nil {
				t.Fatalf("prepare over a stale initializing registration: %v", err)
			}
			defer removePersistentForTest(t, wt)
			gitRun(t, repo, "show-ref", "--verify", "refs/heads/left-behind")
		})
	}
}

// A failed add that did not create the branch must leave it alone.
func TestLocalWorktreeKilledAddKeepsExistingBranch(t *testing.T) {
	repo := newTestRepo(t)
	params := persistentParamsForTest(t, repo, t.TempDir(), testBranchOwner.AgentID)
	wt, err := PrepareLocalWorktree(params, worktreeTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt.Path, "work.txt"), []byte("earlier turn"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Finalize(worktreeTestLogger()); err != nil {
		t.Fatal(err)
	}
	tip := gitRun(t, repo, "rev-parse", "refs/heads/"+wt.Branch)
	removePersistentForTest(t, wt)

	restore := killWorktreeAdd(t)
	if _, err := PrepareLocalWorktree(params, worktreeTestLogger()); err == nil {
		t.Fatal("killed worktree add succeeded")
	}
	restore()
	if got := gitRun(t, repo, "rev-parse", "refs/heads/"+wt.Branch); got != tip {
		t.Fatalf("branch moved from %s to %s", tip, got)
	}
	again, err := PrepareLocalWorktree(params, worktreeTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer removePersistentForTest(t, again)
	if got := readFile(t, filepath.Join(again.Path, "work.txt")); got != "earlier turn" || again.Branch != wt.Branch {
		t.Fatalf("continued %s with %q", again.Branch, got)
	}
}
