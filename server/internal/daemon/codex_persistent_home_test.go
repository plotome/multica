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

	"github.com/multica-ai/multica/server/pkg/agent"
)

func TestRunTaskCodexPersistentHomeTwoTurns(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("persistent enrollment is Unix-only")
	}
	t.Setenv("CODEX_HOME", t.TempDir())
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
	two, err := d.runTask(context.Background(), second, "codex", 0, d.logger)
	if err != nil || two.Status != "completed" || two.SessionID != one.SessionID {
		t.Fatalf("second: %+v %v", two, err)
	}
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	rows := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(rows) != 2 {
		t.Fatalf("expected two executions, got %d", len(rows))
	}
	a, b := strings.Split(rows[0], "|"), strings.Split(rows[1], "|")
	if len(a) != 4 || len(b) != 4 {
		t.Fatal("invalid execution record")
	}
	if a[0] != b[0] || a[1] != first.ID || b[1] != second.ID || a[2] != first.AuthToken || b[2] != second.AuthToken || a[3] == b[3] {
		t.Fatal("home/process/credential invariant failed")
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
