package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
)

// The log callback observes the busy branch synchronously, so tests never
// depend on racing a sleep against lock acquisition.
type homeHandoffLog struct {
	slog.Handler
	onWait func()
}

func (h homeHandoffLog) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == "Codex conversation is still held by the previous run; waiting for it" && h.onWait != nil {
		h.onWait()
	}
	return h.Handler.Handle(ctx, r)
}

func TestCodexHomeHandoff(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("persistent homes are Unix-only")
	}
	for _, kind := range []string{"issue", "chat"} {
		for _, mode := range []string{"release", "cancel", "already-cancelled", "timeout", "quarantine"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				t.Setenv("CODEX_HOME", t.TempDir())
				p := execenv.CodexConversationHomeParams{Profile: "test", WorkspaceID: "workspace", TaskID: "first", Task: execenv.TaskContextForEnv{AgentID: "agent"}}
				if kind == "issue" {
					p.Task.IssueID = "issue"
				} else {
					p.Task.ChatSessionID = "chat"
				}
				old, err := execenv.ClaimCodexConversationHome(p)
				if err != nil || old == nil {
					t.Fatalf("first claim: %v", err)
				}
				defer old.Release()
				if err := old.BindSession("thread"); err != nil {
					t.Fatal(err)
				}
				expectedHome := old.Home()
				p.TaskID, p.ResumeSessionID = "second", "thread"
				ctx, cancel := context.WithCancelCause(context.Background())
				defer cancel(nil)
				cause := errors.New("test run cancelled")
				observed := false
				handler := homeHandoffLog{Handler: slog.NewTextHandler(io.Discard, nil)}
				handler.onWait = func() {
					observed = true
					switch mode {
					case "release":
						old.Release()
					case "cancel":
						cancel(cause)
					case "quarantine":
						old.Quarantine()
					}
				}
				d := &Daemon{logger: slog.New(handler), envRootBusyWait: time.Second}
				if mode == "timeout" {
					d.envRootBusyWait = 0
				}
				if mode == "already-cancelled" {
					cancel(cause)
					old.Release()
				}
				next, err := d.claimCodexHomeAfterPreviousRun(ctx, p)
				if next != nil {
					defer next.Release()
				}
				switch mode {
				case "release":
					if err != nil || next == nil || next.Home() != expectedHome || !observed {
						t.Fatalf("handoff lost resume: home=%v err=%v observed=%v", next, err, observed)
					}
					if extra, e := execenv.ClaimCodexConversationHome(p); e == nil {
						extra.Release()
						t.Fatal("two owners acquired the same conversation")
					}
				case "cancel", "already-cancelled":
					if next != nil || !errors.Is(err, cause) {
						t.Fatalf("cancel did not win: lease=%v err=%v", next, err)
					}
				case "timeout":
					if next != nil || err == nil || !strings.Contains(err.Error(), "wait budget exhausted") {
						t.Fatalf("missing bounded busy failure: %v", err)
					}
				case "quarantine":
					if next != nil || err == nil || !strings.Contains(err.Error(), "unclean execution") || !observed {
						t.Fatalf("quarantine was hidden: %v", err)
					}
				}
			})
		}
	}
}
