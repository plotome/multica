package daemon

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
	"github.com/multica-ai/multica/server/pkg/agent"
)

func TestRunTaskCodexPersistentHomeTwoTurns(t *testing.T) {
	for _, mode := range []string{"warm", "handoff", "cwd-change", "resume-rejected", "rollout-missing", "resume-auth", "unexpected-thread", "retry-config-failure"} {
		t.Run(mode, func(t *testing.T) { testRunTaskCodexPersistentHomeTwoTurns(t, mode) })
	}
}

func testRunTaskCodexPersistentHomeTwoTurns(t *testing.T, mode string) {
	if runtime.GOOS == "windows" {
		t.Skip("persistent enrollment is Unix-only")
	}
	sharedHome := t.TempDir()
	t.Setenv("CODEX_HOME", sharedHome)
	d, _, cleanup := newLeaderReuseTestDaemon(t)
	defer cleanup()
	d.activeStores = make(map[string]int)
	fixture := t.TempDir()
	fake := filepath.Join(fixture, "codex")
	record := filepath.Join(fixture, "executions")
	// A local JSON-RPC fixture, not the user's installed Codex. It writes an
	// opaque rollout plus the effective execution identity for independent checks.
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then echo 'codex-cli 0.153.4'; exit 0; fi
printf '%s|%s|%s|%s\n' "$CODEX_HOME" "$MULTICA_TASK_ID" "$MULTICA_TOKEN" "$$" >> '` + record + `'
mkdir -p "$CODEX_HOME/sessions/2026/09/09"
printf '{}\n' > "$CODEX_HOME/sessions/2026/09/09/rollout-2026-09-09T00-00-00-test-thread.jsonl"
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"thread/start"'*|*'"method":"thread/resume"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"thread":{"id":"test-thread"}}}\n' "$id" ;;
    *'"method":"turn/start"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
      echo '{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"test-thread","turn":{"id":"turn"}}}'
      echo '{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"test-thread","turnId":"turn","item":{"type":"agentMessage","id":"msg","text":"done"}}}'
      echo '{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"test-thread","turn":{"id":"turn","status":"completed"}}}' ;;
    *) if [ -n "$id" ]; then printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"; fi ;;
  esac
done
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	d.cfg.Agents["codex"] = AgentEntry{Path: fake}
	d.runtimeIndex["rt-leader"] = Runtime{ID: "rt-leader", Provider: "codex"}
	first := leaderReuseTestTask("codex-first")
	first.AuthToken = "mat_fixture_first"
	one, err := d.runTask(context.Background(), first, "codex", 0, d.logger)
	if err != nil || one.Status != "completed" || one.SessionID != "test-thread" {
		t.Fatalf("first: %+v %v", one, err)
	}
	second := leaderReuseTestTask("codex-second")
	second.AuthToken = "mat_fixture_second"
	second.PriorSessionID, second.PriorWorkDir = one.SessionID, one.WorkDir
	second.ProjectDescription = "NEW_PROJECT_CONTEXT"
	second.ProjectID = "new-project"
	sameHome := mode == "warm" || mode == "handoff"
	handoffObserved := false
	if mode == "handoff" {
		// Model the interval after server cancellation but before the daemon
		// releases the prior execution's home. Only the observed busy branch
		// releases it, so an immediate-failure implementation cannot pass.
		lease, err := execenv.ClaimCodexConversationHome(execenv.CodexConversationHomeParams{
			Profile: d.cfg.Profile, WorkspaceID: first.WorkspaceID,
			TaskID: first.ID, ResumeSessionID: one.SessionID,
			Task: execenv.TaskContextForEnv{AgentID: first.AgentID, IssueID: first.IssueID, ChatSessionID: first.ChatSessionID},
		})
		if err != nil || lease == nil {
			t.Fatalf("hold prior home: %v", err)
		}
		defer lease.Release()
		d.envRootBusyWait = time.Second
		d.logger = slog.New(homeHandoffLog{Handler: d.logger.Handler(), onWait: func() {
			data, err := os.ReadFile(record)
			if err != nil || len(strings.Split(strings.TrimSpace(string(data)), "\n")) != 1 {
				t.Fatal("new provider started before prior home was released")
			}
			handoffObserved = true
			lease.Release()
		}})
	}
	if mode == "rollout-missing" {
		data, err := os.ReadFile(record)
		if err != nil {
			t.Fatal(err)
		}
		home := strings.Split(strings.TrimSpace(string(data)), "|")[0]
		if err := os.Remove(filepath.Join(home, "sessions/2026/09/09/rollout-2026-09-09T00-00-00-test-thread.jsonl")); err != nil {
			t.Fatal(err)
		}
	} else if !sameHome {
		// Fresh starts return a new thread; a refused resume must not run
		// thread/start in the old provider process or reuse its database.
		script = strings.ReplaceAll(script, "test-thread", "new-thread")
		if mode == "cwd-change" {
			second.PriorWorkDir = t.TempDir()
		}
		if mode == "resume-rejected" || mode == "resume-auth" || mode == "retry-config-failure" {
			script = strings.Replace(script, "case \"$line\" in", `case "$line" in
    *'"method":"thread/resume"'*)
      printf '{"jsonrpc":"2.0","id":%s,"error":{"code":-32600,"message":"thread not found"}}\n' "$id" ;;`, 1)
		}
		if mode == "resume-auth" {
			script = strings.ReplaceAll(script, "thread not found", "authentication failed: 401")
		}
		if mode == "retry-config-failure" {
			// The old process refuses resume, then makes the next preparation
			// fail. It must not erase the already established retirement fact.
			script = strings.Replace(script, `*'"method":"thread/resume"'*)`, `*'"method":"thread/resume"'*)
      printf 'invalid = [' > '`+filepath.Join(sharedHome, "config.toml")+`'`, 1)
		}
		if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	two, err := d.runTask(context.Background(), second, "codex", 0, d.logger)
	if mode == "handoff" && !handoffObserved {
		t.Fatal("runTask bypassed home handoff")
	}
	if mode == "retry-config-failure" {
		if err == nil || !strings.Contains(err.Error(), "prepare fresh Codex home") || two.RetiredSessionID != one.SessionID {
			t.Fatalf("lost retirement on failed retry preparation: %+v %v", two, err)
		}
		return
	}
	if mode == "rollout-missing" || mode == "resume-auth" || mode == "unexpected-thread" {
		if mode == "rollout-missing" {
			if err == nil || !strings.Contains(err.Error(), "persisted Codex rollout is unavailable") {
				t.Fatalf("missing rollout did not fail closed: %+v %v", two, err)
			}
		} else if err != nil || two.Status != "blocked" || two.RetiredSessionID != "" {
			t.Fatalf("unsafe recovery: %+v %v", two, err)
		}
		data, err := os.ReadFile(record)
		if err != nil {
			t.Fatal(err)
		}
		rows := strings.Split(strings.TrimSpace(string(data)), "\n")
		want := 2
		if mode == "rollout-missing" {
			want = 1
		}
		if len(rows) != want {
			t.Fatalf("unsafe retry occurred: %d executions, want %d", len(rows), want)
		}
		return
	}
	wantSession := one.SessionID
	if !sameHome {
		wantSession = "new-thread"
	}
	if err != nil || two.Status != "completed" || two.SessionID != wantSession {
		t.Fatalf("second: %+v %v", two, err)
	}
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	rows := strings.Split(strings.TrimSpace(string(data)), "\n")
	wantExecutions := 2
	if mode == "resume-rejected" {
		wantExecutions = 3
	}
	if len(rows) != wantExecutions {
		t.Fatalf("expected %d executions, got %d", wantExecutions, len(rows))
	}
	a, b := strings.Split(rows[0], "|"), strings.Split(rows[len(rows)-1], "|")
	if len(a) != 4 || len(b) != 4 {
		t.Fatal("invalid execution record")
	}
	if (a[0] == b[0]) != sameHome || a[1] != first.ID || b[1] != second.ID || a[2] != first.AuthToken || b[2] != second.AuthToken || a[3] == b[3] {
		t.Fatal("home/process/credential invariant failed")
	}
	if mode == "resume-rejected" && two.RetiredSessionID != one.SessionID {
		t.Fatal("rejected session not retired")
	}
	brief, err := os.ReadFile(filepath.Join(two.WorkDir, "AGENTS.md"))
	if err != nil || !strings.Contains(string(brief), "NEW_PROJECT_CONTEXT") {
		t.Fatalf("new project context missing: %v", err)
	}
}

func TestExecuteAndDrainPersistentHomeCancellationWaitsForCleanup(t *testing.T) {
	for _, confirmed := range []bool{false, true} {
		d := newTestDaemon(t)
		messages := make(chan agent.Message)
		results := make(chan agent.Result, 1)
		backend := sessionBackend{session: &agent.Session{Messages: messages, Result: results}}
		ctx, cancel := context.WithCancel(context.Background())
		got := make(chan agent.Result, 1)
		go func() {
			r, _, _ := d.executeAndDrain(ctx, backend, "p", agent.ExecOptions{PersistentCodexHome: true}, slog.Default(), "task", "", new(atomic.Int32))
			got <- r
		}()
		messages <- agent.Message{Type: agent.MessageText, Content: "ready"}
		cancel()
		close(messages)
		select {
		case <-got:
			t.Fatal("released before provider cleanup evidence")
		case <-time.After(20 * time.Millisecond):
		}
		results <- agent.Result{Status: "cancelled", ProcessCleanupConfirmed: confirmed}
		select {
		case r := <-got:
			if r.Status != "cancelled" || r.ProcessCleanupConfirmed != confirmed {
				t.Fatalf("lost cleanup evidence: %+v", r)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("cleanup not collected")
		}
	}
}

func TestPersistentHomeFreshRetryRequiresCleanupAndNoTools(t *testing.T) {
	for _, confirmed := range []bool{false, true} {
		for _, toolCount := range []int32{0, 1} {
			result := agent.Result{Status: "failed", ResumeRejected: true, ProcessCleanupConfirmed: confirmed}
			got := shouldRetryWithFreshSessionInEnvironment(result, "old-session", toolCount, "codex", true)
			if got != (confirmed && toolCount == 0) {
				t.Fatalf("retry=%v cleanup=%v tools=%d", got, confirmed, toolCount)
			}
		}
	}
}

// Upstream terminal handoff and the persistent-home cleanup lease must share
// one receive. Consuming Result before the handoff loses the authoritative
// outcome (and can spend a second cleanup budget waiting for it again).
func TestExecuteAndDrainPersistentHomeTerminalHandoff(t *testing.T) {
	probe := &handoffProbe{gate: make(chan struct{}), prefix: "idle watchdog fired; waiting"}
	d := newTestDaemon(t)
	d.cfg.AgentIdleWatchdog = 50 * time.Millisecond
	d.cfg.AgentToolWatchdog = 50 * time.Millisecond
	backend := &lateTerminalBackend{gate: probe.gate}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, _, err := d.executeAndDrain(ctx, backend, "test", agent.ExecOptions{PersistentCodexHome: true}, slog.New(probe), "persistent-terminal", "", new(atomic.Int32))
	if err != nil || !probe.seen.Load() || backend.cancels.Load() == 0 || result.Status != "completed" {
		t.Fatalf("terminal handoff lost: result=%+v err=%v handoff=%v", result, err, probe.seen.Load())
	}
	if result.ProcessCleanupConfirmed {
		t.Fatal("terminal observation must not invent process cleanup evidence")
	}
}

// Cancellation must collect a single provider result for both terminal status
// and cleanup evidence, including a non-authoritative cancellation result.
func TestPersistentHomeCancellationSharesTerminalResult(t *testing.T) {
	for _, authoritative := range []bool{false, true} {
		for _, confirmed := range []bool{false, true} {
			d := newTestDaemon(t)
			messages := make(chan agent.Message)
			results := make(chan agent.Result, 1)
			backend := sessionBackend{session: &agent.Session{
				Messages: messages, Result: results,
				TerminalObserved: func() bool { return authoritative },
			}}
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			got := make(chan agent.Result, 1)
			go func() {
				r, _, _ := d.executeAndDrain(ctx, backend, "p", agent.ExecOptions{PersistentCodexHome: true}, slog.Default(), "task", "", new(atomic.Int32))
				got <- r
			}()
			messages <- agent.Message{Type: agent.MessageText, Content: "ready"}
			cancel()
			close(messages)
			select {
			case <-got:
				t.Fatal("returned before cleanup evidence")
			case <-time.After(20 * time.Millisecond):
			}
			results <- agent.Result{Status: "failed", Error: "provider terminal error", ProcessCleanupConfirmed: confirmed}
			close(results)
			select {
			case r := <-got:
				want := "cancelled"
				if authoritative {
					want = "failed"
				}
				if r.Status != want || r.ProcessCleanupConfirmed != confirmed || (authoritative && r.Error != "provider terminal error") {
					t.Fatalf("authoritative=%v cleanup=%v: result=%+v", authoritative, confirmed, r)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("result handoff did not finish")
			}
		}
	}
}
