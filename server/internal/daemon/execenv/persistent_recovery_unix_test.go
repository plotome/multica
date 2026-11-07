//go:build !windows

package execenv

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestPersistentLocalWorktreeRetryAfterPreparationTimeout(t *testing.T) {
	repo := newTestRepo(t)
	params := persistentParamsForTest(t, repo, t.TempDir(), testBranchOwner.AgentID)
	entry, err := PersistentLocalWorktreeEntryPath(params.PersistentRoot, repo, params.WorkspaceID, params.AgentID, params.ConversationKind, params.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	ready := filepath.Join(bin, "ready")
	// Stop after Git registered the worktree but before checkout/state publication.
	script := fmt.Sprintf(`#!/bin/sh
if [ "$3" = worktree ] && [ "$4" = add ]; then
  root="$2"
  shift 4
  '%s' -C "$root" worktree add --no-checkout "$@" || exit $?
  touch '%s'
  while :; do sleep 1; done
fi
exec '%s' "$@"
`, realGit, ready, realGit)
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(context.Canceled)
	done := make(chan error, 1)
	go func() {
		_, err := PrepareIsolated(ctx, preparationHelperTestCommand("PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH")), PrepareParams{
			WorkspacesRoot: params.PersistentRoot, WorkspaceID: params.WorkspaceID, TaskID: params.TaskID,
			Provider: params.Provider, AgentName: params.AgentName, LocalWorktree: &params,
			Task: TaskContextForEnv{IssueID: params.ConversationID, AgentID: params.AgentID},
		}, worktreeTestLogger())
		done <- err
	}()
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
wait:
	for {
		select {
		case err := <-done:
			t.Fatalf("helper exited before checkout interruption: %v", err)
		case <-deadline.C:
			cancel(context.DeadlineExceeded)
			<-done
			t.Fatal("helper did not reach checkout")
		case <-tick.C:
			if _, err := os.Stat(ready); err == nil {
				break wait
			}
		}
	}
	cancel(context.DeadlineExceeded)
	if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(entry, persistentLocalWorktreeStateFile)); !os.IsNotExist(err) {
		t.Fatalf("expected unpublished state, got %v", err)
	}
	wt, err := PrepareLocalWorktree(params, worktreeTestLogger())
	if err != nil {
		t.Fatalf("retry timed-out helper: %v", err)
	}
	defer removePersistentForTest(t, wt)
	if got := readFile(t, filepath.Join(wt.Path, "tracked.txt")); got != "original\n" {
		t.Fatalf("retry content = %q", got)
	}
}
