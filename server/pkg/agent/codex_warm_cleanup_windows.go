//go:build windows

package agent

import (
	"context"
	"errors"
	"os/exec"
)

func codexWarmHostEnvironment(map[string]string) []string { return nil }

func codexWarmHostingSupported() bool { return false }

// The Windows Job Object can terminate the complete owned tree, but the
// current helper cannot enumerate and remove only post-initialize members.
// Refuse warm hosting rather than carry task-owned processes across turns.
func snapshotCodexWarmProcessGroup(*exec.Cmd) (map[int]struct{}, error) {
	return nil, errors.New("codex warm hosting is unavailable on Windows: selective child cleanup is unsupported")
}

func cleanCodexWarmProcessGroup(context.Context, *exec.Cmd, map[int]struct{}) error {
	return errors.New("codex warm hosting is unavailable on Windows: selective child cleanup is unsupported")
}
