//go:build !windows

package agent

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
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
// app-server itself. Ownership is the union of its process group and live
// descendants, so a turn child that creates a new group/session is still found.
// Any later owned process belongs to a turn and must be gone before the host can
// cross a credential boundary and become idle.
func snapshotCodexWarmProcessGroup(ctx context.Context, cmd *exec.Cmd) (map[int]struct{}, error) {
	if cmd == nil || cmd.Process == nil {
		return nil, errors.New("codex warm host process is not running")
	}
	return codexWarmOwnedProcesses(ctx, cmd.Process.Pid)
}

func cleanCodexWarmProcessGroup(ctx context.Context, cmd *exec.Cmd, baseline map[int]struct{}) error {
	if cmd == nil || cmd.Process == nil || len(baseline) == 0 {
		return errors.New("codex warm host process baseline is unavailable")
	}
	if _, ok := baseline[cmd.Process.Pid]; !ok {
		return errors.New("codex warm host process is absent from its baseline")
	}

	remaining, err := codexWarmExtraProcesses(ctx, cmd.Process.Pid, baseline)
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
		remaining, err = codexWarmExtraProcesses(ctx, cmd.Process.Pid, baseline)
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
		remaining, err := codexWarmExtraProcesses(ctx, groupID, baseline)
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

func codexWarmExtraProcesses(ctx context.Context, groupID int, baseline map[int]struct{}) ([]int, error) {
	members, err := codexWarmOwnedProcesses(ctx, groupID)
	if err != nil {
		return nil, err
	}
	var extra []int
	for pid := range members {
		if _, ok := baseline[pid]; !ok {
			extra = append(extra, pid)
		}
	}
	sort.Ints(extra)
	return extra, nil
}

type codexWarmProcess struct {
	pid   int
	ppid  int
	pgid  int
	state string
}

func codexWarmOwnedProcesses(ctx context.Context, rootPID int) (map[int]struct{}, error) {
	// Route even this OS helper through the package's single command-construction
	// boundary. It has no runtime prefix, and CommandContext makes pool shutdown
	// capable of cancelling a stuck process-table read.
	out, err := NewCommand("ps", nil).exec(ctx, "-axo", "pid=,ppid=,pgid=,stat=").Output()
	if err != nil {
		return nil, fmt.Errorf("list codex warm process tree: %w", err)
	}
	return parseCodexWarmOwnedProcesses(out, rootPID)
}

func parseCodexWarmOwnedProcesses(out []byte, rootPID int) (map[int]struct{}, error) {
	processes := make(map[int]codexWarmProcess)
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 4 {
			continue
		}
		pid, pidErr := strconv.Atoi(fields[0])
		ppid, ppidErr := strconv.Atoi(fields[1])
		pgid, pgidErr := strconv.Atoi(fields[2])
		if pidErr != nil || ppidErr != nil || pgidErr != nil || pid <= 0 {
			continue
		}
		// A zombie has exited and cannot execute code or retain a credential;
		// Linux may keep it visible until the long-lived app-server reaps it.
		// Waiting for it here would deadlock host reuse even though cleanup is
		// already complete. The live parent remains in the baseline separately.
		if strings.HasPrefix(strings.ToUpper(fields[3]), "Z") {
			continue
		}
		processes[pid] = codexWarmProcess{pid: pid, ppid: ppid, pgid: pgid, state: fields[3]}
	}
	if _, ok := processes[rootPID]; !ok {
		return nil, fmt.Errorf("codex warm root process %d is not observable", rootPID)
	}
	owned := map[int]struct{}{rootPID: {}}
	for changed := true; changed; {
		changed = false
		for pid, process := range processes {
			if _, ok := owned[pid]; ok {
				continue
			}
			_, parentOwned := owned[process.ppid]
			if process.pgid == rootPID || parentOwned {
				owned[pid] = struct{}{}
				changed = true
			}
		}
	}
	return owned, nil
}
