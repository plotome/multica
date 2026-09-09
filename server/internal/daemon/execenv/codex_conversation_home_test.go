package execenv

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCodexConversationHomePreparationHelperTwoTurns(t *testing.T) {
	shared := t.TempDir()
	t.Setenv("CODEX_HOME", shared)
	t.Setenv("MULTICA_CODEX_MEMORY", "")
	if err := os.WriteFile(filepath.Join(shared, "config.toml"), []byte("model = \"first-model\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := conversationHomeParams()
	workspace := t.TempDir()
	workdir := t.TempDir()
	var firstHome, firstSkill string
	var firstSkillInfo os.FileInfo
	for round := 0; round < 2; round++ {
		lease, err := ClaimCodexConversationHome(p)
		if err != nil || lease == nil {
			t.Fatalf("claim: %v", err)
		}
		func() {
			defer lease.Release()
			params := PrepareParams{WorkspacesRoot: workspace, WorkspaceID: p.WorkspaceID, TaskID: p.TaskID,
				Provider: "codex", Profile: p.Profile, LocalWorkDir: workdir, CodexConversationHome: lease.Home(), Task: p.Task}
			params.Task.AgentSkills = []SkillContextForEnv{{Name: "test-skill", Content: "# Stable skill\n"}}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			env, err := PrepareIsolated(ctx, preparationHelperTestCommand(), params, testLogger())
			if err != nil {
				t.Fatal(err)
			}
			if env.CodexHome != lease.Home() || env.RootDir == filepath.Dir(env.CodexHome) {
				t.Fatal("state/execution roots not separated")
			}
			config, err := os.ReadFile(filepath.Join(env.CodexHome, "config.toml"))
			if err != nil {
				t.Fatal(err)
			}
			if round == 0 {
				firstHome = env.CodexHome
				firstSkill = filepath.Join(firstHome, "skills", "test-skill", "SKILL.md")
				firstSkillInfo, err = os.Stat(firstSkill)
				if err != nil {
					t.Fatal(err)
				}
				for _, name := range []string{"goals_1.sqlite", "queue_1.sqlite", "thread_history_1.sqlite", "future_state.bin"} {
					if err := os.WriteFile(filepath.Join(firstHome, name), []byte("opaque provider fixture"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.MkdirAll(filepath.Join(firstHome, "shell_snapshots"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(firstHome, "shell_snapshots", "old.sh"), []byte("old task environment"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := lease.BindSession("thread"); err != nil {
					t.Fatal(err)
				}
			} else {
				if env.CodexHome != firstHome {
					t.Fatal("helper changed home")
				}
				fi, err := os.Stat(firstSkill)
				if err != nil {
					t.Fatal(err)
				}
				if !os.SameFile(firstSkillInfo, fi) || !fi.ModTime().Equal(firstSkillInfo.ModTime()) {
					t.Fatal("unchanged skill rewritten")
				}
				if !strings.Contains(string(config), "second-model") || strings.Contains(string(config), "first-model") {
					t.Fatal("stale config")
				}
				if _, err := os.Stat(filepath.Join(firstHome, "shell_snapshots", "old.sh")); !os.IsNotExist(err) {
					t.Fatal("old execution snapshot survived")
				}
				for _, name := range []string{"goals_1.sqlite", "queue_1.sqlite", "thread_history_1.sqlite", "future_state.bin"} {
					if b, err := os.ReadFile(filepath.Join(firstHome, name)); err != nil || string(b) != "opaque provider fixture" {
						t.Fatalf("provider state changed: %s", name)
					}
				}
				// Also exercise managed-workdir Reuse's helper path, not just Prepare.
				reused, err := ReuseIsolated(ctx, preparationHelperTestCommand(), ReuseParams{WorkspacesRoot: workspace, WorkDir: workdir, Provider: "codex", LocalDirectory: true, CodexConversationHome: lease.Home(), Task: params.Task}, testLogger())
				if err != nil || reused == nil || reused.CodexHome != firstHome {
					t.Fatalf("reuse failed: %v", err)
				}
			}
		}()
		p.TaskID, p.ResumeSessionID = "second", "thread"
		if err := os.WriteFile(filepath.Join(shared, "config.toml"), []byte("model = \"second-model\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCodexConversationHomeInvalidConfigFailsClosed(t *testing.T) {
	shared := t.TempDir()
	t.Setenv("CODEX_HOME", shared)
	lease, err := ClaimCodexConversationHome(conversationHomeParams())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if err := os.WriteFile(filepath.Join(shared, "config.toml"), []byte("[broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prepareCodexHomeWithOpts(lease.Home(), CodexHomeOptions{ConversationHome: true}, testLogger()); err == nil {
		t.Fatal("invalid config accepted")
	}
	lease.Release()
	if err := prepareCodexHomeWithOpts(lease.Home(), CodexHomeOptions{ConversationHome: true}, testLogger()); err == nil {
		t.Fatal("unleased home accepted")
	}
}

func conversationHomeParams() CodexConversationHomeParams {
	return CodexConversationHomeParams{Profile: "test", WorkspaceID: "workspace", TaskID: "first", Task: TaskContextForEnv{AgentID: "agent", IssueID: "issue"}}
}

func TestCodexConversationHomeContinuityAndReset(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	p := conversationHomeParams()
	a, err := ClaimCodexConversationHome(p)
	if err != nil || a == nil {
		t.Fatalf("first claim: %v", err)
	}
	if err := os.MkdirAll(a.Home(), 0o700); err != nil {
		t.Fatal(err)
	}
	// Unknown future provider files must survive without a filename allowlist.
	state := filepath.Join(a.Home(), "future-provider-state")
	if err := os.WriteFile(state, []byte("opaque"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.BindSession("thread-1"); err != nil {
		t.Fatal(err)
	}
	firstHome := a.Home()
	a.Release()
	p.TaskID, p.ResumeSessionID = "second", "thread-1"
	b, err := ClaimCodexConversationHome(p)
	if err != nil || b == nil {
		t.Fatalf("resume: %v", err)
	}
	if b.Home() != firstHome {
		t.Fatal("home changed across task IDs")
	}
	if data, err := os.ReadFile(state); err != nil || string(data) != "opaque" {
		t.Fatalf("state lost: %v", err)
	}
	if _, err := ClaimCodexConversationHome(p); err == nil {
		t.Fatal("concurrent owner accepted")
	}
	b.Release()
	p.TaskID, p.ResumeSessionID = "reset", ""
	c, err := ClaimCodexConversationHome(p)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Release()
	if c.Home() == firstHome {
		t.Fatal("explicit fresh session reused old state")
	}
}

func TestCodexConversationHomeIsolationAndLegacy(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	base := conversationHomeParams()
	seen := map[string]bool{}
	for _, change := range []func(*CodexConversationHomeParams){
		func(p *CodexConversationHomeParams) {},
		func(p *CodexConversationHomeParams) { p.Profile = "other" },
		func(p *CodexConversationHomeParams) { p.WorkspaceID = "other" },
		func(p *CodexConversationHomeParams) { p.Task.AgentID = "other" },
		func(p *CodexConversationHomeParams) { p.Task.IssueID = "other" },
		func(p *CodexConversationHomeParams) { p.Task.IssueID = ""; p.Task.ChatSessionID = "issue" },
	} {
		p := base
		change(&p)
		lease, err := ClaimCodexConversationHome(p)
		if err != nil || lease == nil {
			t.Fatalf("claim: %v", err)
		}
		if seen[lease.Home()] {
			t.Fatal("identity collision")
		}
		seen[lease.Home()] = true
		lease.Release()
	}
	base.ResumeSessionID = "pre-upgrade-thread"
	if lease, err := ClaimCodexConversationHome(base); err != nil || lease != nil {
		t.Fatalf("legacy unexpectedly migrated: %v", err)
	}
	base.ResumeSessionID, base.Task.IssueID = "", ""
	if lease, err := ClaimCodexConversationHome(base); err != nil || lease != nil {
		t.Fatalf("unkeyed run persisted: %v", err)
	}
}

func TestCodexConversationHomeUncleanAndMissingFailClosed(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	p := conversationHomeParams()
	a, err := ClaimCodexConversationHome(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.BindSession("thread"); err != nil {
		t.Fatal(err)
	}
	// Simulate a dead daemon without asserting its provider children are dead.
	releaseLockFile(a.lock)
	a.lock = nil
	if _, err := ClaimCodexConversationHome(p); err == nil {
		t.Fatal("unclean owner silently adopted")
	}
	if err := os.Remove(filepath.Join(a.root, "active.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(a.Home()); err != nil {
		t.Fatal(err)
	}
	p.ResumeSessionID = "thread"
	if _, err := ClaimCodexConversationHome(p); err == nil {
		t.Fatal("lost home silently replaced")
	}
}

func TestPruneCodexConversationHomes(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	p := conversationHomeParams()
	a, err := ClaimCodexConversationHome(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.BindSession("thread"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(48 * time.Hour)
	if n, _ := PruneCodexConversationHomes(p.Profile, time.Hour, now, testLogger()); n != 0 {
		t.Fatal("pruned active home")
	}
	a.Release()
	if n, _ := PruneCodexConversationHomes("other", time.Hour, now, testLogger()); n != 0 {
		t.Fatal("cross-profile GC")
	}
	if n, _ := PruneCodexConversationHomes(p.Profile, time.Hour, now, testLogger()); n != 1 {
		t.Fatalf("pruned %d homes", n)
	}
	p.ResumeSessionID = "thread"
	if _, err := ClaimCodexConversationHome(p); err == nil {
		t.Fatal("GC loss treated as legacy resume")
	}
}

func TestCodexConversationHomeRejectsLinkedState(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	p := conversationHomeParams()
	lease, err := ClaimCodexConversationHome(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.BindSession("thread"); err != nil {
		t.Fatal(err)
	}
	home := lease.Home()
	lease.Release()
	if err := os.Remove(home); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, home); err != nil {
		t.Skipf("symlink: %v", err)
	}
	p.ResumeSessionID = "thread"
	if _, err := ClaimCodexConversationHome(p); err == nil {
		t.Fatal("linked provider home accepted")
	}
	if n, _ := PruneCodexConversationHomes(p.Profile, time.Hour, time.Now().Add(48*time.Hour), testLogger()); n != 0 {
		t.Fatal("GC followed linked home")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal("GC removed external target")
	}
}
