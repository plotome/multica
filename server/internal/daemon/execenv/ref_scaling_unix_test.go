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

// Prepare runs under the repository lock, so a git process per historical
// branch makes every later task of the repository wait for all of them.
func TestLocalWorktreePrepareSpawnsDoNotGrowWithBranches(t *testing.T) {
	count := func(branches int) int {
		repo := newTestRepo(t)
		head := gitRun(t, repo, "rev-parse", "HEAD")
		var refs strings.Builder
		for i := 0; i < branches; i++ {
			fmt.Fprintf(&refs, "create refs/heads/agent/x/old-%d %s\n", i, head)
			fmt.Fprintf(&refs, "create %sagent/x/old-%d %s\n", localStateRefPrefix, i, head)
			fmt.Fprintf(&refs, "create %sagent/x/old-%d/key %s\n", leftoverRefPrefix, i, head)
		}
		update := exec.Command("git", "-C", repo, "update-ref", "--stdin")
		update.Stdin = strings.NewReader(refs.String())
		if out, err := update.CombinedOutput(); err != nil {
			t.Fatalf("create refs: %v: %s", err, out)
		}
		realGit, err := exec.LookPath("git")
		if err != nil {
			t.Fatal(err)
		}
		bin := t.TempDir()
		calls := filepath.Join(bin, "calls")
		script := fmt.Sprintf("#!/bin/sh\necho x >> '%s'\nexec '%s' \"$@\"\n", calls, realGit)
		if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		path := os.Getenv("PATH")
		t.Setenv("PATH", bin+string(os.PathListSeparator)+path)
		defer t.Setenv("PATH", path)
		params := persistentParamsForTest(t, repo, t.TempDir(), testBranchOwner.AgentID)
		wt, err := PrepareLocalWorktree(params, worktreeTestLogger())
		if err != nil {
			t.Fatal(err)
		}
		got := strings.Count(readFile(t, calls), "x")
		removePersistentForTest(t, wt)
		return got
	}
	if few, many := count(2), count(30); few != many {
		t.Fatalf("prepare ran git %d times with 2 branches and %d times with 30", few, many)
	}
}

func TestPruneOrphanedRefsDropsOnlyDeletedBranches(t *testing.T) {
	repo := newTestRepo(t)
	head := gitRun(t, repo, "rev-parse", "HEAD")
	gitRun(t, repo, "branch", "agent/x/kept")
	for _, ref := range []string{
		localStateRefPrefix + "agent/x/kept", localStateRefPrefix + "agent/x/gone",
		leftoverRefPrefix + "agent/x/kept/key", leftoverRefPrefix + "agent/x/gone/key",
	} {
		gitRun(t, repo, "update-ref", ref, head)
	}
	pruneOrphanedStateRefs(repo, worktreeTestLogger())
	got := gitRun(t, repo, "for-each-ref", "--format=%(refname)", localStateRefPrefix, leftoverRefPrefix)
	want := leftoverRefPrefix + "agent/x/kept/key\n" + localStateRefPrefix + "agent/x/kept"
	if got != want {
		t.Fatalf("remaining refs:\n%s\nwant:\n%s", got, want)
	}
}

// A conversation whose branch names are all taken by branches it does not own
// still has to write a record its own next turn can read.
func TestPersistentLocalWorktreeFallbackBranchStaysReadable(t *testing.T) {
	repo := newTestRepo(t)
	params := persistentParamsForTest(t, repo, t.TempDir(), testBranchOwner.AgentID)
	preferred := "agent/j/mul-123"
	gitRun(t, repo, "branch", preferred)
	gitRun(t, repo, "branch", preferred+"-"+params.owner().fingerprint())

	first, err := PrepareLocalWorktree(params, worktreeTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(first.Path, "work.txt"), []byte("turn one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Finalize(worktreeTestLogger()); err != nil {
		t.Fatal(err)
	}
	params.TaskID = "99998888-7777-6666-5555-444433332222"
	second, err := PrepareLocalWorktree(params, worktreeTestLogger())
	if err != nil {
		t.Fatalf("second turn: %v", err)
	}
	defer removePersistentForTest(t, second)
	if second.Branch != first.Branch || readFile(t, filepath.Join(second.Path, "work.txt")) != "turn one" {
		t.Fatalf("second turn continued %s, want %s", second.Branch, first.Branch)
	}
}
