package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/multica-ai/multica/server/pkg/agent"
)

const (
	defaultWarmSessionMaxPerProvider = 10
	defaultWarmSessionIdleTTL        = 24 * time.Hour
	defaultWarmSessionSweepInterval  = time.Minute
)

var (
	errWarmSessionPoolClosed         = errors.New("warm session pool is closed")
	errWarmSessionCleanupUnconfirmed = errors.New("warm session cleanup could not be confirmed")
)

type warmEntryState uint8

const (
	warmEntryStarting warmEntryState = iota
	warmEntryLeased
	warmEntryIdle
	warmEntryClosing
	warmEntryQuarantined
)

type warmPoolEntry struct {
	key         string
	fingerprint string
	host        agent.WarmHost
	state       warmEntryState
	idleSince   time.Time
}

// warmSessionPool owns one Codex host per conversation. entries is also the
// strict capacity ledger: starting, closing, and quarantined hosts remain in
// the map until cleanup is positively confirmed.
type warmSessionPool struct {
	mu      sync.Mutex
	maxLive int
	idleTTL time.Duration
	now     func() time.Time
	entries map[string]*warmPoolEntry
	closing bool
	changed chan struct{}
}

func newWarmSessionPool(maxLive int, idleTTL time.Duration) *warmSessionPool {
	if maxLive <= 0 {
		maxLive = defaultWarmSessionMaxPerProvider
	}
	if idleTTL <= 0 {
		idleTTL = defaultWarmSessionIdleTTL
	}
	return &warmSessionPool{
		maxLive: maxLive,
		idleTTL: idleTTL,
		now:     time.Now,
		entries: make(map[string]*warmPoolEntry),
		changed: make(chan struct{}),
	}
}

func (p *warmSessionPool) notifyLocked() {
	close(p.changed)
	p.changed = make(chan struct{})
}

type warmSessionLease struct {
	pool  *warmSessionPool
	entry *warmPoolEntry
	once  sync.Once
}

func (l *warmSessionLease) Host() agent.WarmHost { return l.entry.host }

func (l *warmSessionLease) Release(ctx context.Context, reusable bool) error {
	var err error
	l.once.Do(func() { err = l.pool.release(ctx, l.entry, reusable) })
	return err
}

// acquire serializes a conversation, performs LRU eviction at capacity, and
// runs host creation/closure outside the mutex.
func (p *warmSessionPool) acquire(
	ctx context.Context,
	key string,
	fingerprint string,
	create func(context.Context) (agent.WarmHost, error),
) (*warmSessionLease, bool, error) {
	if key == "" {
		return nil, false, errors.New("warm session key is empty")
	}
	if create == nil {
		return nil, false, errors.New("warm session host factory is nil")
	}

	for {
		p.mu.Lock()
		if p.closing {
			p.mu.Unlock()
			return nil, false, errWarmSessionPoolClosed
		}
		if entry := p.entries[key]; entry != nil {
			switch entry.state {
			case warmEntryIdle:
				if entry.fingerprint == fingerprint {
					entry.state = warmEntryLeased
					entry.idleSince = time.Time{}
					p.mu.Unlock()
					return &warmSessionLease{pool: p, entry: entry}, true, nil
				}
				entry.state = warmEntryClosing
				p.mu.Unlock()
				if err := p.closeEntry(ctx, entry); err != nil {
					return nil, false, err
				}
				continue
			case warmEntryQuarantined:
				entry.state = warmEntryClosing
				p.mu.Unlock()
				if err := p.closeEntry(ctx, entry); err != nil {
					return nil, false, err
				}
				continue
			default:
				changed := p.changed
				p.mu.Unlock()
				if err := waitWarmPoolChange(ctx, changed); err != nil {
					return nil, false, err
				}
				continue
			}
		}

		if len(p.entries) < p.maxLive {
			entry := &warmPoolEntry{key: key, fingerprint: fingerprint, state: warmEntryStarting}
			p.entries[key] = entry
			p.mu.Unlock()
			host, createErr := create(ctx)

			p.mu.Lock()
			entry.host = host
			closing := p.closing
			if createErr == nil && host != nil && !closing {
				entry.state = warmEntryLeased
				p.notifyLocked()
				p.mu.Unlock()
				return &warmSessionLease{pool: p, entry: entry}, false, nil
			}
			if host == nil {
				delete(p.entries, key)
				p.notifyLocked()
				p.mu.Unlock()
				return nil, false, warmCreateError(createErr, closing)
			}
			entry.state = warmEntryClosing
			p.mu.Unlock()
			closeErr := p.closeEntry(context.Background(), entry)
			return nil, false, errors.Join(warmCreateError(createErr, closing), closeErr)
		}

		victim := p.oldestIdleLocked()
		if victim != nil {
			victim.state = warmEntryClosing
			p.mu.Unlock()
			if err := p.closeEntry(ctx, victim); err != nil {
				return nil, false, err
			}
			continue
		}
		if victim = p.firstQuarantinedLocked(); victim != nil {
			victim.state = warmEntryClosing
			p.mu.Unlock()
			if err := p.closeEntry(ctx, victim); err != nil {
				return nil, false, err
			}
			continue
		}
		changed := p.changed
		p.mu.Unlock()
		if err := waitWarmPoolChange(ctx, changed); err != nil {
			return nil, false, err
		}
	}
}

func warmCreateError(createErr error, closing bool) error {
	if createErr != nil {
		return fmt.Errorf("start warm session host: %w", createErr)
	}
	if closing {
		return errWarmSessionPoolClosed
	}
	return errors.New("warm session host factory returned nil")
}

func waitWarmPoolChange(ctx context.Context, changed <-chan struct{}) error {
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-changed:
		return nil
	}
}

func (p *warmSessionPool) oldestIdleLocked() *warmPoolEntry {
	var oldest *warmPoolEntry
	for _, entry := range p.entries {
		if entry.state == warmEntryIdle && (oldest == nil || entry.idleSince.Before(oldest.idleSince)) {
			oldest = entry
		}
	}
	return oldest
}

func (p *warmSessionPool) firstQuarantinedLocked() *warmPoolEntry {
	for _, entry := range p.entries {
		if entry.state == warmEntryQuarantined {
			return entry
		}
	}
	return nil
}

func (p *warmSessionPool) allQuarantinedLocked() bool {
	if len(p.entries) == 0 {
		return false
	}
	for _, entry := range p.entries {
		if entry.state != warmEntryQuarantined {
			return false
		}
	}
	return true
}

func (p *warmSessionPool) release(ctx context.Context, entry *warmPoolEntry, reusable bool) error {
	p.mu.Lock()
	if p.entries[entry.key] != entry || entry.state != warmEntryLeased {
		p.mu.Unlock()
		return nil
	}
	if reusable && !p.closing {
		entry.state = warmEntryIdle
		entry.idleSince = p.now()
		p.notifyLocked()
		p.mu.Unlock()
		return nil
	}
	entry.state = warmEntryClosing
	p.mu.Unlock()
	return p.closeEntry(ctx, entry)
}

func (p *warmSessionPool) closeEntry(ctx context.Context, entry *warmPoolEntry) error {
	err := entry.host.Close(ctx)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.entries[entry.key] != entry {
		return err
	}
	if err != nil {
		entry.state = warmEntryQuarantined
		p.notifyLocked()
		return fmt.Errorf("%w: %v", errWarmSessionCleanupUnconfirmed, err)
	}
	delete(p.entries, entry.key)
	p.notifyLocked()
	return nil
}

func (p *warmSessionPool) sweep(ctx context.Context) error {
	now := p.now()
	p.mu.Lock()
	var expired []*warmPoolEntry
	for _, entry := range p.entries {
		if entry.state == warmEntryIdle && now.Sub(entry.idleSince) >= p.idleTTL {
			entry.state = warmEntryClosing
			expired = append(expired, entry)
		}
	}
	p.mu.Unlock()
	var errs []error
	for _, entry := range expired {
		if err := p.closeEntry(ctx, entry); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (p *warmSessionPool) close(ctx context.Context) error {
	p.mu.Lock()
	p.closing = true
	p.notifyLocked()
	var closeNow []*warmPoolEntry
	// Every caller may retry a previously quarantined host. Close on the host is
	// idempotent and can finish after an earlier caller's context expired.
	for _, entry := range p.entries {
		if entry.state == warmEntryLeased || entry.state == warmEntryIdle || entry.state == warmEntryQuarantined {
			entry.state = warmEntryClosing
			closeNow = append(closeNow, entry)
		}
	}
	p.mu.Unlock()

	var errs []error
	for _, entry := range closeNow {
		if err := p.closeEntry(ctx, entry); err != nil {
			errs = append(errs, err)
		}
	}
	for {
		p.mu.Lock()
		if len(p.entries) == 0 {
			p.mu.Unlock()
			return errors.Join(errs...)
		}
		if p.allQuarantinedLocked() {
			p.mu.Unlock()
			return errors.Join(append(errs, errWarmSessionCleanupUnconfirmed)...)
		}
		changed := p.changed
		p.mu.Unlock()
		if err := waitWarmPoolChange(ctx, changed); err != nil {
			return errors.Join(append(errs, err)...)
		}
	}
}

type warmPoolStats struct {
	Live        int
	Busy        int
	Idle        int
	Starting    int
	Closing     int
	Quarantined int
}

func (p *warmSessionPool) stats() warmPoolStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	stats := warmPoolStats{Live: len(p.entries)}
	for _, entry := range p.entries {
		switch entry.state {
		case warmEntryStarting:
			stats.Starting++
		case warmEntryLeased:
			stats.Busy++
		case warmEntryIdle:
			stats.Idle++
		case warmEntryClosing:
			stats.Closing++
		case warmEntryQuarantined:
			stats.Quarantined++
		}
	}
	return stats
}
