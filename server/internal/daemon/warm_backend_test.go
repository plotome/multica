package daemon

import (
	"context"
	"errors"
	"io"
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

type fakeAgentWarmHost struct {
	executes   atomic.Int32
	prepares   atomic.Int32
	closes     atomic.Int32
	healthy    atomic.Bool
	prepareErr error
	closeErr   error
}

func newFakeAgentWarmHost() *fakeAgentWarmHost {
	h := &fakeAgentWarmHost{}
	h.healthy.Store(true)
	return h
}

func (h *fakeAgentWarmHost) Healthy() bool { return h.healthy.Load() }
func (h *fakeAgentWarmHost) PrepareIdle(context.Context) error {
	h.prepares.Add(1)
	return h.prepareErr
}
func (h *fakeAgentWarmHost) Close(context.Context) error {
	h.closes.Add(1)
	if h.closeErr == nil {
		h.healthy.Store(false)
	}
	return h.closeErr
}

func TestPinnedWarmHostRetainsEnvironmentUntilCleanupConfirmed(t *testing.T) {
	host := newFakeAgentWarmHost()
	host.closeErr = errors.New("still alive")
	var unpins atomic.Int32
	pinned := &pinnedWarmHost{WarmHost: host, onClose: func() { unpins.Add(1) }}
	if err := pinned.Close(t.Context()); err == nil || unpins.Load() != 0 {
		t.Fatalf("first close error=%v unpins=%d", err, unpins.Load())
	}
	host.closeErr = nil
	if err := pinned.Close(t.Context()); err != nil || unpins.Load() != 1 {
		t.Fatalf("confirmed close error=%v unpins=%d", err, unpins.Load())
	}
}
func (h *fakeAgentWarmHost) Execute(context.Context, string, agent.ExecOptions) (*agent.Session, error) {
	n := h.executes.Add(1)
	messages := make(chan agent.Message)
	results := make(chan agent.Result, 1)
	close(messages)
	results <- agent.Result{Status: "completed", SessionID: "thread", Output: string(rune('0' + n))}
	close(results)
	return &agent.Session{Messages: messages, Result: results}, nil
}

func TestPooledCodexBackendReusesHealthyHost(t *testing.T) {
	pool := newWarmSessionPool(10, time.Hour)
	host := newFakeAgentWarmHost()
	var creates atomic.Int32
	backend := &pooledCodexBackend{
		pool: pool, key: "conversation", fingerprint: "config",
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		factory: func(context.Context) (agent.WarmHost, error) {
			creates.Add(1)
			return host, nil
		},
	}
	for i := 0; i < 2; i++ {
		session, err := backend.Execute(t.Context(), "prompt", agent.ExecOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for range session.Messages {
		}
		result := <-session.Result
		if result.Status != "completed" {
			t.Fatalf("result = %#v", result)
		}
	}
	if creates.Load() != 1 || host.executes.Load() != 2 || host.prepares.Load() != 2 {
		t.Fatalf("creates=%d executes=%d prepares=%d, want 1, 2 and 2", creates.Load(), host.executes.Load(), host.prepares.Load())
	}
	if err := pool.close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if host.closes.Load() != 1 {
		t.Fatalf("closes=%d, want 1", host.closes.Load())
	}
}

func TestPooledCodexBackendDiscardsHostWhenIdlePreparationFails(t *testing.T) {
	pool := newWarmSessionPool(10, time.Hour)
	first := newFakeAgentWarmHost()
	first.prepareErr = context.DeadlineExceeded
	second := newFakeAgentWarmHost()
	var creates atomic.Int32
	backend := &pooledCodexBackend{
		pool: pool, key: "conversation", fingerprint: "config",
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		factory: func(context.Context) (agent.WarmHost, error) {
			if creates.Add(1) == 1 {
				return first, nil
			}
			return second, nil
		},
	}
	for i := 0; i < 2; i++ {
		session, err := backend.Execute(t.Context(), "prompt", agent.ExecOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for range session.Messages {
		}
		if result := <-session.Result; result.Status != "completed" {
			t.Fatalf("result = %#v", result)
		}
	}
	if creates.Load() != 2 || first.closes.Load() != 1 || second.executes.Load() != 1 {
		t.Fatalf("creates=%d first closes=%d second executes=%d", creates.Load(), first.closes.Load(), second.executes.Load())
	}
	if err := pool.close(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestPooledCodexBackendFailsClosedWhenCleanupIsUnconfirmed(t *testing.T) {
	pool := newWarmSessionPool(1, time.Hour)
	host := newFakeAgentWarmHost()
	host.prepareErr = errors.New("turn child still alive")
	host.closeErr = errors.New("host tree still alive")
	fallback := newFakeAgentWarmHost()
	backend := &pooledCodexBackend{
		pool: pool, key: "conversation", fingerprint: "config", fallback: fallback,
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		factory: func(context.Context) (agent.WarmHost, error) { return host, nil },
	}

	session, err := backend.Execute(t.Context(), "prompt", agent.ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for range session.Messages {
	}
	result := <-session.Result
	if result.Status != "failed" || !strings.Contains(result.Error, errWarmSessionCleanupUnconfirmed.Error()) {
		t.Fatalf("result = %#v, want failed cleanup confirmation", result)
	}
	if stats := pool.stats(); stats.Live != 1 || stats.Quarantined != 1 {
		t.Fatalf("pool stats = %#v, want retained quarantine", stats)
	}
	if _, err := backend.Execute(t.Context(), "next", agent.ExecOptions{}); !errors.Is(err, errWarmSessionCleanupUnconfirmed) {
		t.Fatalf("next execute error = %v, want cleanup-unconfirmed", err)
	}
	if fallback.executes.Load() != 0 {
		t.Fatal("cold fallback must not race an unconfirmed warm process")
	}
}

func TestPooledCodexBackendRebuildsHostThatExitedWhileIdle(t *testing.T) {
	pool := newWarmSessionPool(1, time.Hour)
	first := newFakeAgentWarmHost()
	second := newFakeAgentWarmHost()
	var creates atomic.Int32
	backend := &pooledCodexBackend{
		pool: pool, key: "conversation", fingerprint: "config",
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		factory: func(context.Context) (agent.WarmHost, error) {
			if creates.Add(1) == 1 {
				return first, nil
			}
			return second, nil
		},
	}

	for range mustExecuteWarmBackend(t, backend).Messages {
	}
	first.healthy.Store(false)
	session := mustExecuteWarmBackend(t, backend)
	for range session.Messages {
	}
	if result := <-session.Result; result.Status != "completed" {
		t.Fatalf("replacement result = %#v", result)
	}
	if creates.Load() != 2 || first.closes.Load() != 1 || second.executes.Load() != 1 {
		t.Fatalf("creates=%d first closes=%d second executes=%d", creates.Load(), first.closes.Load(), second.executes.Load())
	}
	if err := pool.close(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func mustExecuteWarmBackend(t *testing.T, backend *pooledCodexBackend) *agent.Session {
	t.Helper()
	session, err := backend.Execute(t.Context(), "prompt", agent.ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func TestPooledCodexBackendFallsBackWhenWarmStartupFails(t *testing.T) {
	pool := newWarmSessionPool(10, time.Hour)
	fallback := newFakeAgentWarmHost()
	backend := &pooledCodexBackend{
		pool: pool, key: "conversation", fingerprint: "config", fallback: fallback,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		factory: func(context.Context) (agent.WarmHost, error) {
			return nil, errors.New("warm unavailable")
		},
	}
	session, err := backend.Execute(t.Context(), "prompt", agent.ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for range session.Messages {
	}
	if result := <-session.Result; result.Status != "completed" || fallback.executes.Load() != 1 {
		t.Fatalf("result=%#v fallback executes=%d", result, fallback.executes.Load())
	}
	if stats := pool.stats(); stats.Live != 0 {
		t.Fatalf("pool stats after failed warm startup = %#v", stats)
	}
}

func TestPooledCodexBackendRetriesCatalogStartupAfterReap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture is POSIX-only")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "second-launch")
	fake := filepath.Join(dir, "codex")
	script := `#!/bin/sh
second=false
if [ -f "$STATE" ]; then second=true; else touch "$STATE"; fi
read init
echo '{"jsonrpc":"2.0","id":1,"result":{}}'
read initialized
read start
echo '{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"thr-retry"}}}'
read turn
echo '{"jsonrpc":"2.0","id":3,"result":{}}'
echo '{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"thr-retry","turn":{"id":"turn-retry"}}}'
if [ "$second" = true ]; then
  echo '{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"thr-retry","turnId":"turn-retry","item":{"type":"agentMessage","id":"msg-retry","phase":"final_answer","text":"recovered"}}}'
  echo '{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"thr-retry","turn":{"id":"turn-retry","status":"completed"}}}'
else
  echo 'failed to refresh available models: timeout waiting for child process to exit' >&2
  read interrupt
  echo '{"jsonrpc":"2.0","id":4,"result":{}}'
fi
while read rest; do :; done
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	opts := agent.ExecOptions{FirstTurnNoProgressTimeout: 50 * time.Millisecond, SemanticInactivityTimeout: 100 * time.Millisecond}
	cfg := agent.Config{ExecutablePath: fake, Env: map[string]string{"STATE": marker}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	pool := newWarmSessionPool(1, time.Hour)
	backend := &pooledCodexBackend{
		pool: pool, key: "conversation", fingerprint: "config", logger: cfg.Logger,
		factory: func(ctx context.Context) (agent.WarmHost, error) {
			return agent.NewWarmHost(ctx, "codex", cfg, opts)
		},
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	session, err := backend.Execute(ctx, "prompt", opts)
	if err != nil {
		t.Fatal(err)
	}
	for range session.Messages {
	}
	result := <-session.Result
	if result.Status != "completed" || result.Output != "recovered" {
		t.Fatalf("result = %#v", result)
	}
	if err := pool.close(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestCodexWarmEligibilityFailsClosedForTaskScopedMCP(t *testing.T) {
	task := Task{IssueID: "issue", WorkspaceID: "workspace", RuntimeID: "runtime", AgentID: "agent"}
	env := &execenv.Environment{RootDir: "/root", WorkDir: "/root/workdir", CodexHome: t.TempDir()}
	if !codexWarmEligible(task, agent.ExecOptions{}, env, false) {
		t.Fatal("plain built-in Codex task should be warm eligible")
	}
	if codexWarmEligible(task, agent.ExecOptions{McpConfig: []byte(`{}`)}, env, false) {
		t.Fatal("managed MCP must bypass warm hosting")
	}
	task.ConnectedApps = []ConnectedAppData{{}}
	if codexWarmEligible(task, agent.ExecOptions{}, env, false) {
		t.Fatal("connected app broker must bypass warm hosting")
	}
	task.ConnectedApps = nil
	task.PluginHookTools = []PluginHookTool{{}}
	if codexWarmEligible(task, agent.ExecOptions{}, env, false) {
		t.Fatal("plugin hook MCP must bypass warm hosting")
	}
	task.PluginHookTools = nil
	if codexWarmEligible(Task{IssueID: "issue"}, agent.ExecOptions{}, env, true) {
		t.Fatal("custom runtime profile must bypass warm hosting")
	}
	for _, args := range [][]string{
		{"-c", "mcp_servers.demo.command='demo'"},
		{"-c", `mcp_servers={probe={command="probe"}}`},
		{"--config=mcp_servers = { probe = { command = \"probe\" } }"},
	} {
		if codexWarmEligible(task, agent.ExecOptions{CustomArgs: args}, env, false) {
			t.Fatalf("custom-arg MCP %q must bypass warm hosting", args)
		}
	}
	if !codexWarmEligible(task, agent.ExecOptions{CustomArgs: []string{"-c", `model="gpt-5"`}}, env, false) {
		t.Fatal("unrelated Codex config override should remain warm eligible")
	}
	codexHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte("[mcp_servers.demo]\ncommand = 'demo'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env.CodexHome = codexHome
	if codexWarmEligible(task, agent.ExecOptions{}, env, false) {
		t.Fatal("inherited config MCP must bypass warm hosting")
	}
}

func TestCodexTurnShellEnvironmentUsesCurrentExplicitValues(t *testing.T) {
	got := codexTurnShellEnvironment(
		[]string{"PATH=/bin", "MULTICA_TOKEN=daemon-token", "API_KEY=daemon-secret", "LANG=en_US.UTF-8"},
		map[string]string{"MULTICA_TOKEN": "mat_task", "MULTICA_TASK_ID": "task-2", "API_KEY": "agent-secret"},
		[]string{"API_KEY"},
	)
	if got["MULTICA_TOKEN"] != "mat_task" || got["MULTICA_TASK_ID"] != "task-2" || got["API_KEY"] != "agent-secret" {
		t.Fatalf("current explicit values missing: %#v", got)
	}
	if got["PATH"] != "/bin" || got["LANG"] != "en_US.UTF-8" {
		t.Fatalf("safe inherited values missing: %#v", got)
	}
	if got["MULTICA_TOKEN"] == "daemon-token" || got["API_KEY"] == "daemon-secret" {
		t.Fatalf("stale inherited secret escaped: %#v", got)
	}
}
