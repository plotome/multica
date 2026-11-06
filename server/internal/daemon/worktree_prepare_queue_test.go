package daemon

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRunTaskWorktreeQueueDoesNotSpendPrepareBudget(t *testing.T) {
	path := t.TempDir()
	waiting := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "wait-local-directory") {
			select {
			case waiting <- struct{}{}:
			default:
			}
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	d := &Daemon{client: NewClient(srv.URL), localPathLocks: NewLocalPathLocker(), taskPrepareTimeout: 20 * time.Millisecond,
		cfg: Config{DaemonID: "queue-daemon", Agents: map[string]AgentEntry{}}}
	ref, _ := json.Marshal(localDirectoryRef{DaemonID: d.cfg.DaemonID, LocalPath: path, ExecutionMode: "worktree"})
	task := Task{ID: "queued-task", WorkspaceID: "ws", AgentID: "agent", Agent: &AgentData{ID: "agent"},
		ProjectResources: []ProjectResourceData{{ResourceType: localDirectoryResourceType, ResourceRef: ref}}}
	assignment, err := localDirectoryAssignmentForTask(task, d.cfg.DaemonID)
	if err != nil {
		t.Fatal(err)
	}
	release, err := d.localPathLocks.Acquire(context.Background(), assignment.RealPath, "holder", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := d.runTask(ctx, task, "missing-provider", 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("task exited before queue release: %v", err)
	case <-waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("task never entered preparation queue")
	}
	select {
	case err := <-done:
		t.Fatalf("queue consumed preparation budget: %v", err)
	case <-time.After(3 * d.taskPrepareTimeout):
	}
	release()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "missing-provider") {
			t.Fatalf("after queue: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("task did not leave queue")
	}
}
