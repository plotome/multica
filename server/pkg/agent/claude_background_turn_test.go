package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Claude Code sends the SDK hook callbacks in every turn, including the turns
// a one-shot process opens on its own when a background task reports. If the
// daemon has closed stdin, the callback cannot be answered and the CLI cancels
// the tool call with "The user doesn't want to take this action right now",
// which the agent reads as the user stopping it.
func TestClaudeBackgroundTurnKeepsHookInputOpen(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake CLI fixture is POSIX-oriented")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	run := func(t *testing.T, variant, resumeID string) Result {
		t.Helper()
		b, err := New("claude", Config{
			ExecutablePath: self,
			Logger:         discard,
			Env: map[string]string{
				"CLAUDE_FAKE_MODE":               "background_turn",
				"CLAUDE_FAKE_BACKGROUND_VARIANT": variant,
				"IS_SANDBOX":                     "1",
			},
		})
		if err != nil {
			t.Fatalf("New(claude): %v", err)
		}
		sess, err := b.Execute(context.Background(), "do the work", ExecOptions{
			Cwd:                  t.TempDir(),
			Model:                "claude-opus-5-5",
			ResumeSessionID:      resumeID,
			Timeout:              10 * time.Second,
			EnableTaskSupplement: true,
			HandshakeTimeout:     2 * time.Second,
		})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		for range sess.Messages {
		}
		return <-sess.Result
	}

	t.Run("notification turn after the result", func(t *testing.T) {
		r := run(t, "after_result", "")
		if r.Status != "completed" || r.Output != "notification turn ran its tool" {
			t.Fatalf("result = %+v, want the notification turn's tool answered", r)
		}
	})

	t.Run("task running when the turn ends", func(t *testing.T) {
		r := run(t, "task_outlives_turn", "")
		if r.Status != "completed" || r.Output != "notification turn ran its tool" {
			t.Fatalf("result = %+v, want the notification turn's tool answered", r)
		}
	})

	// On --resume, a task the previous process left running is reported as
	// stopped and answered before the queued prompt starts. That result is
	// not this turn's; taking it would close stdin under the real turn.
	t.Run("orphaned task notification on resume", func(t *testing.T) {
		r := run(t, "resume_orphan", "claude-resumed")
		if r.Status != "completed" || r.Output != "reply to do the work" {
			t.Fatalf("result = %+v, want the reply to the queued prompt", r)
		}
	})
}

// runFakeClaudeBackgroundTurn mirrors the background-task frames Claude Code
// 2.1.332 emits. Every hook waits for its reply; one that cannot be answered
// exits 41, the way the CLI cancels the tool call.
func runFakeClaudeBackgroundTurn() {
	frames := make(chan map[string]any, 16)
	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Buffer(make([]byte, 1<<20), 1<<20)
		for scanner.Scan() {
			var frame map[string]any
			if json.Unmarshal(scanner.Bytes(), &frame) == nil {
				frames <- frame
			}
		}
		close(frames)
	}()
	out := json.NewEncoder(os.Stdout)
	emit := func(frame map[string]any) {
		frame["session_id"] = "claude-background"
		_ = out.Encode(frame)
	}
	var queued []map[string]any
	next := func(match func(map[string]any) bool) (map[string]any, bool) {
		deadline := time.After(5 * time.Second)
		for {
			select {
			case frame, ok := <-frames:
				if !ok {
					return nil, false
				}
				if match(frame) {
					return frame, true
				}
				if frame["type"] == "user" {
					queued = append(queued, frame)
				}
			case <-deadline:
				os.Exit(40)
			}
		}
	}
	hook := func(requestID, event string) {
		msg := claudeSupplementHook(event, "")
		msg.RequestID = requestID
		_ = out.Encode(msg)
		if _, ok := next(func(f map[string]any) bool {
			resp, _ := f["response"].(map[string]any)
			return f["type"] == "control_response" && resp["request_id"] == requestID
		}); !ok {
			os.Exit(41)
		}
	}
	tasks := func(ids ...string) {
		list := []any{}
		for _, id := range ids {
			list = append(list, map[string]any{"task_id": id, "task_type": "local_bash"})
		}
		emit(map[string]any{"type": "system", "subtype": "background_tasks_changed", "tasks": list})
	}
	notify := func(id string) {
		emit(map[string]any{"type": "system", "subtype": "task_notification", "task_id": id, "status": "completed"})
	}
	assistant := func(text string) {
		emit(map[string]any{"type": "assistant", "message": map[string]any{
			"id": "msg-" + text, "role": "assistant", "model": "claude-opus-5-5",
			"content": []any{map[string]any{"type": "text", "text": text}},
		}})
	}
	result := func(text string, origin map[string]any) {
		frame := map[string]any{"type": "result", "subtype": "success", "result": text, "is_error": false}
		if origin != nil {
			frame["origin"] = origin
		}
		emit(frame)
	}
	notificationOrigin := map[string]any{"kind": "task-notification", "producer": "session-task"}
	notificationTurn := func(n string) {
		hook("ups-"+n, "UserPromptSubmit")
		hook("pre-"+n, "PreToolUse")
		assistant("notification turn ran its tool")
		hook("stop-"+n, "Stop")
		result("notification turn ran its tool", notificationOrigin)
	}

	first, ok := next(func(map[string]any) bool { return true })
	if !ok {
		os.Exit(10)
	}
	if first["type"] == "control_request" {
		_ = out.Encode(map[string]any{"type": "control_response", "response": map[string]any{
			"subtype": "success", "request_id": first["request_id"], "response": map[string]any{},
		}})
	} else {
		queued = append(queued, first)
	}
	variant := os.Getenv("CLAUDE_FAKE_BACKGROUND_VARIANT")
	if variant == "resume_orphan" {
		emit(map[string]any{"type": "system", "subtype": "task_notification", "task_id": "orphan", "status": "stopped"})
		result("", notificationOrigin)
	}
	var prompt map[string]any
	if len(queued) > 0 {
		prompt = queued[0]
	} else if prompt, ok = next(func(f map[string]any) bool { return f["type"] == "user" }); !ok {
		os.Exit(11)
	}
	id, _ := prompt["uuid"].(string)
	emit(map[string]any{"type": "command_lifecycle", "command_uuid": id, "state": "queued"})
	emit(map[string]any{"type": "command_lifecycle", "command_uuid": id, "state": "started"})
	hook("ups-own", "UserPromptSubmit")
	hook("pre-own", "PreToolUse")
	switch variant {
	case "after_result", "task_outlives_turn":
		tasks("bg1")
	}
	reply := "reply to " + promptText(prompt)
	assistant(reply)
	hook("stop-own", "Stop")
	result(reply, nil)
	emit(map[string]any{"type": "command_lifecycle", "command_uuid": id, "state": "completed"})

	switch variant {
	case "after_result":
		// The task ends after the turn: notification, then the empty list,
		// then the turn the CLI opens for it.
		notify("bg1")
		tasks()
		notificationTurn("1")
	case "task_outlives_turn":
		// A second task starts inside the notification turn of the first
		// and reports later in a turn of its own.
		notify("bg1")
		emit(map[string]any{"type": "system", "subtype": "background_tasks_changed", "tasks": []any{
			map[string]any{"task_id": "bg2", "task_type": "local_bash"},
		}})
		notificationTurn("1")
		notify("bg2")
		tasks()
		notificationTurn("2")
	}
	// Like the CLI, exit once stdin reaches EOF with nothing left to run.
	for range frames {
	}
}

func promptText(frame map[string]any) string {
	message, _ := frame["message"].(map[string]any)
	content, _ := message["content"].([]any)
	if len(content) == 0 {
		return ""
	}
	block, _ := content[0].(map[string]any)
	text, _ := block["text"].(string)
	return strings.TrimSpace(text)
}
