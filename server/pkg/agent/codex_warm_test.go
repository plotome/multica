package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCodexWarmHostReusesProcessAndRefreshesTurnEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture is POSIX-only")
	}
	record := filepath.Join(t.TempDir(), "requests.jsonl")
	fake := writeFakeCodexAppServer(t, ""+
		`read init; printf '%s\n' "$init" >> "$RECORD"`+"\n"+
		`echo '{"jsonrpc":"2.0","id":1,"result":{}}'`+"\n"+
		`read initialized`+"\n"+
		`read start1; printf '%s\n' "$start1" >> "$RECORD"`+"\n"+
		`echo '{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"thr-warm"}}}'`+"\n"+
		`read turn1`+"\n"+
		`echo '{"jsonrpc":"2.0","id":3,"result":{}}'`+"\n"+
		`echo '{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"thr-warm","turn":{"id":"turn-1"}}}'`+"\n"+
		`echo '{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"thr-warm","turnId":"turn-1","item":{"type":"agentMessage","id":"msg-1","phase":"final_answer","text":"first"}}}'`+"\n"+
		`echo '{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"thr-warm","turn":{"id":"turn-1","status":"completed"}}}'`+"\n"+
		`read resume; printf '%s\n' "$resume" >> "$RECORD"`+"\n"+
		`echo '{"jsonrpc":"2.0","id":4,"result":{"thread":{"id":"thr-warm"}}}'`+"\n"+
		`read turn2`+"\n"+
		`echo '{"jsonrpc":"2.0","id":5,"result":{}}'`+"\n"+
		`echo '{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"thr-warm","turn":{"id":"turn-2"}}}'`+"\n"+
		`echo '{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"thr-warm","turnId":"turn-2","item":{"type":"agentMessage","id":"msg-2","phase":"final_answer","text":"second"}}}'`+"\n"+
		`echo '{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"thr-warm","turn":{"id":"turn-2","status":"completed"}}}'`+"\n"+
		`while read rest; do :; done`+"\n")

	host, err := NewWarmHost(t.Context(), "codex", Config{
		ExecutablePath: fake,
		Env:            map[string]string{"RECORD": record},
	}, ExecOptions{Cwd: t.TempDir(), CodexShellEnv: map[string]string{"MULTICA_TOKEN": "mat_first"}})
	if err != nil {
		t.Fatalf("start warm host: %v", err)
	}
	t.Cleanup(func() { _ = host.Close(context.Background()) })

	first := executeWarmTurn(t, host, "one", ExecOptions{
		Cwd: t.TempDir(), CodexShellEnv: map[string]string{"MULTICA_TOKEN": "mat_first"},
		SemanticInactivityTimeout: time.Second,
	})
	if first.Status != "completed" || first.Output != "first" || first.SessionID != "thr-warm" {
		t.Fatalf("first result = %#v", first)
	}
	if err := host.PrepareIdle(t.Context()); err != nil {
		t.Fatalf("prepare host after first turn: %v", err)
	}
	second := executeWarmTurn(t, host, "two", ExecOptions{
		Cwd: t.TempDir(), ResumeSessionID: first.SessionID,
		CodexShellEnv:             map[string]string{"MULTICA_TOKEN": "mat_second", "MULTICA_TASK_ID": "task-2"},
		SemanticInactivityTimeout: time.Second,
	})
	if second.Status != "completed" || second.Output != "second" {
		t.Fatalf("second result = %#v", second)
	}
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Fatalf("recorded %d requests, want initialize + start + resume: %s", len(lines), data)
	}
	if !strings.Contains(lines[2], `"method":"thread/resume"`) ||
		!strings.Contains(lines[2], `"MULTICA_TOKEN":"mat_second"`) ||
		!strings.Contains(lines[2], `"MULTICA_TASK_ID":"task-2"`) ||
		strings.Contains(lines[2], "mat_first") {
		t.Fatalf("resume did not carry only current task environment: %s", lines[2])
	}
}

func TestCodexWarmHostPrepareIdleKillsTurnBackgroundProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("warm hosting deliberately fails closed on Windows")
	}
	childFile := filepath.Join(t.TempDir(), "child.pid")
	fake := writeFakeCodexAppServer(t, ""+
		`read init`+"\n"+
		`echo '{"jsonrpc":"2.0","id":1,"result":{}}'`+"\n"+
		`read initialized`+"\n"+
		`read start`+"\n"+
		`echo '{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"thr-child"}}}'`+"\n"+
		`read turn`+"\n"+
		`sleep 30 & echo $! > "$CHILD_FILE"`+"\n"+
		`echo '{"jsonrpc":"2.0","id":3,"result":{}}'`+"\n"+
		`echo '{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"thr-child","turn":{"id":"turn-child"}}}'`+"\n"+
		`echo '{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"thr-child","turnId":"turn-child","item":{"type":"agentMessage","id":"msg-child","phase":"final_answer","text":"done"}}}'`+"\n"+
		`echo '{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"thr-child","turn":{"id":"turn-child","status":"completed"}}}'`+"\n"+
		`while read rest; do :; done`+"\n")
	host, err := NewWarmHost(t.Context(), "codex", Config{
		ExecutablePath: fake,
		Env:            map[string]string{"CHILD_FILE": childFile},
	}, ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close(context.Background()) })
	result := executeWarmTurn(t, host, "prompt", ExecOptions{SemanticInactivityTimeout: time.Second})
	if result.Status != "completed" {
		t.Fatalf("result = %#v", result)
	}
	pidText, err := os.ReadFile(childFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidText)))
	if err != nil {
		t.Fatal(err)
	}
	if err := host.PrepareIdle(t.Context()); err != nil {
		t.Fatalf("prepare idle: %v", err)
	}
	if !codexWarmTestProcessGone(pid) {
		t.Fatalf("turn background pid %d still exists", pid)
	}
}

func TestCodexWarmHostEnvironmentDropsTaskIdentity(t *testing.T) {
	t.Setenv("MULTICA_TOKEN", "daemon-token")
	safe := codexWarmSanitizedEnvironment(map[string]string{
		"CODEX_HOME": "/codex", "OPENAI_API_KEY": "stable-key",
		"MULTICA_TOKEN": "task-token", "MULTICA_TASK_ID": "task", "TMPDIR": "/task/tmp",
	})
	if safe["CODEX_HOME"] != "/codex" || safe["OPENAI_API_KEY"] != "stable-key" {
		t.Fatalf("stable host environment missing: %#v", safe)
	}
	for _, key := range []string{"MULTICA_TOKEN", "MULTICA_TASK_ID", "TMPDIR"} {
		if _, ok := safe[key]; ok {
			t.Fatalf("dynamic %s retained in sanitized host environment", key)
		}
	}
	for _, entry := range codexWarmHostEnvironment(safe) {
		if strings.HasPrefix(strings.ToUpper(entry), "MULTICA_") {
			t.Fatalf("inherited Multica credential reached warm host: %q", entry)
		}
	}
}

func TestCodexWarmHostMarksCatalogNoProgressForRetry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture is POSIX-only")
	}
	fake := writeFakeCodexAppServer(t, ""+
		`read init`+"\n"+
		`echo '{"jsonrpc":"2.0","id":1,"result":{}}'`+"\n"+
		`read initialized`+"\n"+
		`read start`+"\n"+
		`echo '{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"thr-catalog"}}}'`+"\n"+
		`read turn`+"\n"+
		`echo '{"jsonrpc":"2.0","id":3,"result":{}}'`+"\n"+
		`echo '{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"thr-catalog","turn":{"id":"turn-catalog"}}}'`+"\n"+
		`echo 'failed to refresh available models: timeout waiting for child process to exit' >&2`+"\n"+
		`read interrupt`+"\n"+
		`echo '{"jsonrpc":"2.0","id":4,"result":{}}'`+"\n"+
		`while read rest; do :; done`+"\n")
	host, err := NewWarmHost(t.Context(), "codex", Config{ExecutablePath: fake}, ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	result := executeWarmTurn(t, host, "prompt", ExecOptions{
		SemanticInactivityTimeout:  100 * time.Millisecond,
		FirstTurnNoProgressTimeout: 50 * time.Millisecond,
	})
	if result.Status != "timeout" || !CodexWarmStartupRetryCandidate(result) {
		t.Fatalf("result = %#v; want retryable catalog timeout", result)
	}
	if host.Healthy() {
		t.Fatal("timed-out warm host must be poisoned")
	}
	if err := host.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCodexWarmHostCancellationDuringResumePoisonsHost(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture is POSIX-only")
	}
	fake := writeFakeCodexAppServer(t, ""+
		`read init`+"\n"+
		`echo '{"jsonrpc":"2.0","id":1,"result":{}}'`+"\n"+
		`read initialized`+"\n"+
		`read start`+"\n"+
		`while read rest; do :; done`+"\n")
	host, err := NewWarmHost(t.Context(), "codex", Config{ExecutablePath: fake}, ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	session, err := host.Execute(ctx, "prompt", ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	for range session.Messages {
	}
	result := <-session.Result
	if result.Status != "aborted" || host.Healthy() {
		t.Fatalf("result=%#v healthy=%v; want aborted poisoned host", result, host.Healthy())
	}
	if err := host.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCodexWarmHostCloseHonorsContextAndCanBeObservedLater(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture is POSIX-only")
	}
	// Not parallel: this test overrides the package-wide graceful timeout.
	codexGracefulShutdownTimeoutNanos.Store(int64(100 * time.Millisecond))
	t.Cleanup(func() { codexGracefulShutdownTimeoutNanos.Store(0) })
	fake := writeFakeCodexAppServer(t, ""+
		`read init`+"\n"+
		`echo '{"jsonrpc":"2.0","id":1,"result":{}}'`+"\n"+
		`read initialized`+"\n"+
		`while :; do sleep 1; done`+"\n")
	host, err := NewWarmHost(t.Context(), "codex", Config{ExecutablePath: fake}, ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	if err := host.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("short close error = %v, want context deadline", err)
	}
	if err := host.Close(context.Background()); err != nil {
		t.Fatalf("observe completed close: %v", err)
	}
}

func executeWarmTurn(t *testing.T, host WarmHost, prompt string, opts ExecOptions) Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	session, err := host.Execute(ctx, prompt, opts)
	if err != nil {
		t.Fatalf("execute warm turn: %v", err)
	}
	go func() {
		for range session.Messages {
		}
	}()
	select {
	case result := <-session.Result:
		return result
	case <-ctx.Done():
		t.Fatal("warm turn timed out")
		return Result{}
	}
}
