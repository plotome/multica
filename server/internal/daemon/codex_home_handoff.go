package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
)

// Server-side cancellation precedes local provider cleanup. The home lease is
// acquired before the prior-workdir wait, so it must tolerate the same handoff
// window. Reuse that existing budget and cadence; never fall back to a fresh
// home when the old one is busy or quarantined.
func (d *Daemon) claimCodexHomeAfterPreviousRun(ctx context.Context, p execenv.CodexConversationHomeParams) (*execenv.CodexConversationHomeLease, error) {
	start := time.Now()
	deadline := start.Add(d.envRootBusyWait)
	waited := false
	for {
		if err := context.Cause(ctx); err != nil {
			return nil, err
		}
		lease, err := execenv.ClaimCodexConversationHome(p)
		// Cancellation may arrive while touching the filesystem. No provider has
		// started, so a newly acquired lease can still be released safely.
		if cause := context.Cause(ctx); cause != nil {
			if lease != nil {
				lease.Release()
			}
			return nil, cause
		}
		if !errors.Is(err, execenv.ErrCodexConversationBusy) {
			if waited && err == nil {
				d.logger.Info("Codex conversation freed while waiting for the previous run to exit", "task", p.TaskID, "waited", time.Since(start))
			}
			return lease, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, fmt.Errorf("Codex conversation handoff wait budget exhausted after %s: %w", time.Since(start).Round(time.Millisecond), err)
		}
		if !waited {
			waited = true
			d.logger.Info("Codex conversation is still held by the previous run; waiting for it", "task", p.TaskID, "budget", d.envRootBusyWait)
		}
		timer := time.NewTimer(min(envRootBusyRetryInterval, remaining))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, context.Cause(ctx)
		case <-timer.C:
		}
	}
}
