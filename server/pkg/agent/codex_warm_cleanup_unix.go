//go:build !windows

package agent

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const codexWarmChildCleanupGrace = 500 * time.Millisecond

func codexWarmHostingSupported() bool { return true }

func codexWarmHostEnvironment(extra map[string]string) []string {
	base := make([]string, 0, len(buildEnv(nil)))
	for _, entry := range buildEnv(nil) {
		key, _, ok := strings.Cut(entry, "=")
		if ok && !strings.HasPrefix(strings.ToUpper(key), "MULTICA_") {
			base = append(base, entry)
		}
	}
	return mergeEnv(base, extra)
}

// snapshotCodexWarmProcessGroup records the processes needed by the initialized
// app-server itself. Any later member of the group belongs to a turn and must
// be gone before the host can cross a credential boundary and become idle.
func snapshotCodexWarmProcessGroup(cmd *exec.Cmd) (map[int]struct{}, error) {
	if cmd == nil || cmd.Process == nil {
		return nil, errors.New("codex warm host process is not running")
	}
	return codexWarmProcessGroupMembers(cmd.Process.Pid)
}

func cleanCodexWarmProcessGroup(ctx context.Context, cmd *exec.Cmd, baseline map[int]struct{}) error {
	if cmd == nil || cmd.Process == nil || len(baseline) == 0 {
		return errors.New("codex warm host process baseline is unavailable")
	}
	if _, ok := baseline[cmd.Process.Pid]; !ok {
		return errors.New("codex warm host process is absent from its baseline")
	}

	remaining, err := codexWarmExtraProcesses(cmd.Process.Pid, baseline)
	if err != nil || len(remaining) == 0 {
		return err
	}
	for _, pid := range remaining {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}

	graceTimer := time.NewTimer(codexWarmChildCleanupGrace)
	defer graceTimer.Stop()
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	for {
		remaining, err = codexWarmExtraProcesses(cmd.Process.Pid, baseline)
		if err != nil {
			return err
		}
		if len(remaining) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("clean codex warm child processes: %w", context.Cause(ctx))
		case <-graceTimer.C:
			for _, pid := range remaining {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
			return waitCodexWarmExtraProcessesGone(ctx, cmd.Process.Pid, baseline)
		case <-poll.C:
		}
	}
}

func waitCodexWarmExtraProcessesGone(ctx context.Context, groupID int, baseline map[int]struct{}) error {
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	for {
		remaining, err := codexWarmExtraProcesses(groupID, baseline)
		if err != nil {
			return err
		}
		if len(remaining) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("confirm codex warm child cleanup (remaining pids %v): %w", remaining, context.Cause(ctx))
		case <-poll.C:
		}
	}
}

func codexWarmExtraProcesses(groupID int, baseline map[int]struct{}) ([]int, error) {
	members, err := codexWarmProcessGroupMembers(groupID)
	if err != nil {
		return nil, err
	}
	var extra []int
	for pid := range members {
		if _, ok := baseline[pid]; !ok {
			extra = append(extra, pid)
		}
	}
	return extra, nil
}

func codexWarmProcessGroupMembers(groupID int) (map[int]struct{}, error) {
	out, err := exec.Command("ps", "-axo", "pid=,pgid=,stat=").Output()
	if err != nil {
		return nil, fmt.Errorf("list process groups: %w", err)
	}
	return parseCodexWarmProcessGroupMembers(out, groupID)
}

func parseCodexWarmProcessGroupMembers(out []byte, groupID int) (map[int]struct{}, error) {
	members := make(map[int]struct{})
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		pgid, pgidErr := strconv.Atoi(fields[1])
		if pgidErr != nil || pgid != groupID {
			continue
		}
		// A zombie has exited and cannot execute code or retain a credential;
		// Linux may keep it visible until the long-lived app-server reaps it.
		// Waiting for it here would deadlock host reuse even though cleanup is
		// already complete. The live parent remains in the baseline separately.
		if strings.HasPrefix(strings.ToUpper(fields[2]), "Z") {
			continue
		}
		pid, pidErr := strconv.Atoi(fields[0])
		if pidErr == nil && pid > 0 {
			members[pid] = struct{}{}
		}
	}
	if len(members) == 0 {
		return nil, fmt.Errorf("process group %d is not observable", groupID)
	}
	return members, nil
}
