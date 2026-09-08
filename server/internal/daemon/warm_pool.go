package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	defaultWarmSessionMaxPerProvider = 10
	defaultWarmSessionIdleTTL        = 24 * time.Hour
	defaultWarmSessionSweepInterval  = time.Minute
)

var errWarmSessionPoolClosed = errors.New("warm session pool is closed")

// warmSessionHost is a provider process which can serve multiple sequential
// turns for one conversation. A host is never shared by two conversations.
type warmSessionHost interface {
	Close(context.Context) error
}

type warmPoolEntry struct {
	key         string
	fingerprint string
	host        warmSessionHost
	busy        bool
	starting    bool
	idleSince   time.Time
}

// warmSessionPool owns the live provider processes for one provider. maxLive
// is a strict process ceiling: starting and closing hosts consume a slot too.
// This is intentionally different from an ordinary cache size, where an
// eviction can remove an entry before the resource has actually stopped.
type warmSessionPool struct {
	mu      sync.Mutex
	maxLive int
	idleTTL time.Duration
	now     func() time.Time
	entries map[string]*warmPoolEntry
	live    int
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

// warmSessionLease gives one task exclusive use of a conversation host.
// Release must be called exactly once. reusable=false poisons and closes the
// host before returning its strict-capacity slot to another waiter.
type warmSessionLease struct {
	pool  *warmSessionPool
	entry *warmPoolEntry
	once  sync.Once
}

func (l *warmSessionLease) Host() warmSessionHost { return l.entry.host }

func (l *warmSessionLease) Release(ctx context.Context, reusable bool) error {
	var releaseErr error
	l.once.Do(func() {
		releaseErr = l.pool.release(ctx, l.entry, reusable)
	})
	return releaseErr
}

// acquire serialises turns for the same key, evicts the least-recently-used
// idle host when the provider is at capacity, and otherwise waits rather than
// exceeding maxLive. create runs without the pool mutex held.
func (p *warmSessionPool) acquire(
	ctx context.Context,
	key string,
	fingerprint string,
	create func(context.Context) (warmSessionHost, error),
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
			switch {
			case entry.fingerprint != fingerprint:
				if entry.busy || entry.starting {
					changed := p.changed
					p.mu.Unlock()
					if err := waitWarmPoolChange(ctx, changed); err != nil {
						return nil, false, err
					}
					continue
				}
				delete(p.entries, key)
				p.mu.Unlock()
				p.closeHost(ctx, entry.host)
				continue
			case entry.busy || entry.starting:
				changed := p.changed
				p.mu.Unlock()
				if err := waitWarmPoolChange(ctx, changed); err != nil {
					return nil, false, err
				}
				continue
			default:
				entry.busy = true
				entry.idleSince = time.Time{}
				p.mu.Unlock()
				return &warmSessionLease{pool: p, entry: entry}, true, nil
			}
		}

		if p.live < p.maxLive {
			entry := &warmPoolEntry{key: key, fingerprint: fingerprint, busy: true, starting: true}
			p.entries[key] = entry
			p.live++
			p.mu.Unlock()

			host, err := create(ctx)
			p.mu.Lock()
			entry.starting = false
			if err != nil || p.closing {
				delete(p.entries, key)
				closing := p.closing
				p.mu.Unlock()
				if host != nil {
					_ = host.Close(context.Background())
				}
				p.mu.Lock()
				p.live--
				p.notifyLocked()
				p.mu.Unlock()
				if err != nil {
					return nil, false, fmt.Errorf("start warm session host: %w", err)
				}
				if closing {
					return nil, false, errWarmSessionPoolClosed
				}
				return nil, false, errors.New("warm session host factory returned nil")
			}
			if host == nil {
				delete(p.entries, key)
				p.live--
				p.notifyLocked()
				p.mu.Unlock()
				return nil, false, errors.New("warm session host factory returned nil")
			}
			entry.host = host
			p.notifyLocked()
			p.mu.Unlock()
			return &warmSessionLease{pool: p, entry: entry}, false, nil
		}

		victim := p.oldestIdleLocked()
		if victim == nil {
			changed := p.changed
			p.mu.Unlock()
			if err := waitWarmPoolChange(ctx, changed); err != nil {
				return nil, false, err
			}
			continue
		}
		delete(p.entries, victim.key)
		p.mu.Unlock()
		p.closeHost(ctx, victim.host)
	}
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
		if entry.busy || entry.starting || entry.host == nil {
			continue
		}
		if oldest == nil || entry.idleSince.Before(oldest.idleSince) {
			oldest = entry
		}
	}
	return oldest
}

func (p *warmSessionPool) release(ctx context.Context, entry *warmPoolEntry, reusable bool) error {
	p.mu.Lock()
	current := p.entries[entry.key]
	if current != entry {
		p.mu.Unlock()
		return nil
	}
	if reusable && !p.closing {
		entry.busy = false
		entry.idleSince = p.now()
		p.notifyLocked()
		p.mu.Unlock()
		return nil
	}
	delete(p.entries, entry.key)
	p.mu.Unlock()
	return p.closeHost(ctx, entry.host)
}

func (p *warmSessionPool) closeHost(ctx context.Context, host warmSessionHost) error {
	var err error
	if host != nil {
		err = host.Close(ctx)
	}
	p.mu.Lock()
	p.live--
	p.notifyLocked()
	p.mu.Unlock()
	return err
}

// sweep closes idle hosts whose TTL has elapsed. Busy hosts are never evicted.
func (p *warmSessionPool) sweep(ctx context.Context) error {
	now := p.now()
	var expired []*warmPoolEntry
	p.mu.Lock()
	for key, entry := range p.entries {
		if entry.busy || entry.starting || entry.idleSince.IsZero() || now.Sub(entry.idleSince) < p.idleTTL {
			continue
		}
		delete(p.entries, key)
		expired = append(expired, entry)
	}
	p.mu.Unlock()

	var errs []error
	for _, entry := range expired {
		if err := p.closeHost(ctx, entry.host); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (p *warmSessionPool) close(ctx context.Context) error {
	p.mu.Lock()
	if p.closing {
		p.mu.Unlock()
		for {
			p.mu.Lock()
			if p.live == 0 {
				p.mu.Unlock()
				return nil
			}
			changed := p.changed
			p.mu.Unlock()
			if err := waitWarmPoolChange(ctx, changed); err != nil {
				return err
			}
		}
	}
	p.closing = true
	p.notifyLocked()
	entries := make([]*warmPoolEntry, 0, len(p.entries))
	for key, entry := range p.entries {
		if entry.starting || entry.busy {
			continue
		}
		delete(p.entries, key)
		entries = append(entries, entry)
	}
	p.mu.Unlock()

	var errs []error
	for _, entry := range entries {
		if err := p.closeHost(ctx, entry.host); err != nil {
			errs = append(errs, err)
		}
	}

	// Active leases close themselves on release after observing p.closing.
	for {
		p.mu.Lock()
		if p.live == 0 {
			p.notifyLocked()
			p.mu.Unlock()
			return errors.Join(errs...)
		}
		changed := p.changed
		p.mu.Unlock()
		if err := waitWarmPoolChange(ctx, changed); err != nil {
			return errors.Join(append(errs, err)...)
		}
	}
}

type warmPoolStats struct {
	Live     int
	Busy     int
	Idle     int
	Starting int
}

func (p *warmSessionPool) stats() warmPoolStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	stats := warmPoolStats{Live: p.live}
	for _, entry := range p.entries {
		switch {
		case entry.starting:
			stats.Starting++
		case entry.busy:
			stats.Busy++
		default:
			stats.Idle++
		}
	}
	return stats
}
