package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestCodexPersistentHomePreLaunchFailureNeedsNoRecovery(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix-only positive launch evidence")
	}
	backend, err := New("codex", Config{ExecutablePath: filepath.Join(t.TempDir(), "missing-codex"), Logger: slog.Default()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = backend.Execute(context.Background(), "p", ExecOptions{PersistentCodexHome: true})
	if err == nil || !CodexProcessNeverStarted(err) {
		t.Fatalf("missing pre-launch evidence: %v", err)
	}
}

func TestCodexPersistentHomeNeverFallsBackFromResume(t *testing.T) {
	for _, response := range []rpcResponse{
		{method: "thread/resume", errMsg: "unknown thread", errCode: -32602},
		{method: "thread/resume", result: json.RawMessage(`{"thread":{}}`)},
		{method: "thread/resume", result: json.RawMessage(`{"thread":{"id":"different"}}`)},
	} {
		c, fs, _ := newTestCodexClient(t)
		wait := drainRPCScript(t, c, fs, []rpcResponse{response})
		thread, resumed, err := c.startOrResumeThread(context.Background(), ExecOptions{Cwd: "/work", ResumeSessionID: "original", PersistentCodexHome: true}, slog.Default())
		wait()
		if err == nil || resumed || thread != "" {
			t.Fatalf("resume silently replaced: %q %v %v", thread, resumed, err)
		}
		if len(fs.Lines()) != 1 {
			t.Fatal("unexpected thread/start")
		}
	}
}

func TestCodexPersistentHomeReportsProcessCleanup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fixture")
	}
	for _, confirmed := range []bool{true, false} {
		if !confirmed {
			codexCleanupConfirmationOverride.Store(-1)
		}
		t.Cleanup(func() { codexCleanupConfirmationOverride.Store(0) })
		fake := writeFakeCodexAppServer(t, `read line
echo '{"jsonrpc":"2.0","id":1,"result":{}}'
read line
read line
echo '{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"thread"}}}'
read line
echo '{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"thread","turn":{"id":"turn"}}}'
echo '{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"thread","turnId":"turn","item":{"type":"agentMessage","id":"msg","text":"done"}}}'
echo '{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"thread","turn":{"id":"turn","status":"completed"}}}'
`)
		result := executeFakeCodex(t, fake, ExecOptions{PersistentCodexHome: true, ResumeSessionID: "thread", Timeout: 5 * time.Second})
		if result.Status != "completed" || result.ProcessCleanupConfirmed != confirmed {
			t.Fatalf("cleanup evidence: status=%s cleanup=%v want=%v error=%s", result.Status, result.ProcessCleanupConfirmed, confirmed, result.Error)
		}
		codexCleanupConfirmationOverride.Store(0)
	}
}
