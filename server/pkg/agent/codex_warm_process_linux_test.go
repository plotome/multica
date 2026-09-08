//go:build linux

package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCodexWarmHostPrepareIdleKillsDetachedTurnDescendant(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid is unavailable")
	}
	childFile := filepath.Join(t.TempDir(), "child.pid")
	fake := writeFakeCodexAppServer(t, ""+
		`read init`+"\n"+
		`echo '{"jsonrpc":"2.0","id":1,"result":{}}'`+"\n"+
		`read initialized`+"\n"+
		`read start`+"\n"+
		`echo '{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"thr-detached"}}}'`+"\n"+
		`read turn`+"\n"+
		`setsid sleep 30 & echo $! > "$CHILD_FILE"`+"\n"+
		`echo '{"jsonrpc":"2.0","id":3,"result":{}}'`+"\n"+
		`echo '{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"thr-detached","turn":{"id":"turn-detached"}}}'`+"\n"+
		`echo '{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"thr-detached","turnId":"turn-detached","item":{"type":"agentMessage","id":"msg-detached","phase":"final_answer","text":"done"}}}'`+"\n"+
		`echo '{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"thr-detached","turn":{"id":"turn-detached","status":"completed"}}}'`+"\n"+
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
		t.Fatalf("detached turn descendant pid %d still exists", pid)
	}
}
