package daemon

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/pkg/agent"
)

type fakeWarmHost struct {
	id       string
	closed   atomic.Int32
	closeErr error
}

func (h *fakeWarmHost) Close(context.Context) error {
	h.closed.Add(1)
	return h.closeErr
}

func (h *fakeWarmHost) Healthy() bool                     { return h.closeErr == nil && h.closed.Load() == 0 }
func (h *fakeWarmHost) PrepareIdle(context.Context) error { return nil }
func (h *fakeWarmHost) Execute(context.Context, string, agent.ExecOptions) (*agent.Session, error) {
	panic("fakeWarmHost.Execute must not be called by pool tests")
}

func TestWarmSessionPoolQuarantinesUnconfirmedCleanup(t *testing.T) {
	pool := newWarmSessionPool(1, time.Hour)
	host := &fakeWarmHost{id: "stuck", closeErr: errors.New("still alive")}
	lease, _, err := pool.acquire(t.Context(), "first", "config", func(context.Context) (agent.WarmHost, error) {
		return host, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(t.Context(), false); !errors.Is(err, errWarmSessionCleanupUnconfirmed) {
		t.Fatalf("release error = %v, want cleanup-unconfirmed", err)
	}
	if stats := pool.stats(); stats.Live != 1 || stats.Quarantined != 1 {
		t.Fatalf("quarantined pool stats = %#v, want one live quarantined slot", stats)
	}
	var creates atomic.Int32
	_, _, err = pool.acquire(t.Context(), "second", "config", func(context.Context) (agent.WarmHost, error) {
		creates.Add(1)
		return &fakeWarmHost{id: "unsafe replacement"}, nil
	})
	if !errors.Is(err, errWarmSessionCleanupUnconfirmed) || creates.Load() != 0 {
		t.Fatalf("replacement acquire error=%v creates=%d", err, creates.Load())
	}

	// A later positive reap confirmation releases both the slot and the entry.
	host.closeErr = nil
	replacement, hit, err := pool.acquire(t.Context(), "second", "config", func(context.Context) (agent.WarmHost, error) {
		creates.Add(1)
		return &fakeWarmHost{id: "safe replacement"}, nil
	})
	if err != nil || hit || creates.Load() != 1 {
		t.Fatalf("replacement acquire = hit %v, err %v, creates %d", hit, err, creates.Load())
	}
	if err := replacement.Release(t.Context(), false); err != nil {
		t.Fatal(err)
	}
}

func TestWarmSessionPoolCloseRetriesQuarantinedHost(t *testing.T) {
	pool := newWarmSessionPool(1, time.Hour)
	host := &fakeWarmHost{id: "stuck", closeErr: errors.New("still alive")}
	lease, _, err := pool.acquire(t.Context(), "conversation", "config", func(context.Context) (agent.WarmHost, error) {
		return host, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(t.Context(), false); !errors.Is(err, errWarmSessionCleanupUnconfirmed) {
		t.Fatalf("release error = %v, want cleanup-unconfirmed", err)
	}
	if err := pool.close(t.Context()); !errors.Is(err, errWarmSessionCleanupUnconfirmed) {
		t.Fatalf("first close error = %v, want cleanup-unconfirmed", err)
	}

	host.closeErr = nil
	if err := pool.close(t.Context()); err != nil {
		t.Fatalf("retry close: %v", err)
	}
	if stats := pool.stats(); stats.Live != 0 {
		t.Fatalf("pool stats after confirmed retry = %#v, want empty", stats)
	}
}

func TestWarmSessionPoolReusesSameConversation(t *testing.T) {
	pool := newWarmSessionPool(10, 24*time.Hour)
	var creates atomic.Int32
	create := func(context.Context) (agent.WarmHost, error) {
		creates.Add(1)
		return &fakeWarmHost{id: "one"}, nil
	}

	first, hit, err := pool.acquire(t.Context(), "conversation", "config", create)
	if err != nil || hit {
		t.Fatalf("first acquire = hit %v, err %v; want miss", hit, err)
	}
	host := first.Host()
	if err := first.Release(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	second, hit, err := pool.acquire(t.Context(), "conversation", "config", create)
	if err != nil || !hit {
		t.Fatalf("second acquire = hit %v, err %v; want hit", hit, err)
	}
	if second.Host() != host || creates.Load() != 1 {
		t.Fatalf("host was not reused; creates = %d", creates.Load())
	}
	_ = second.Release(t.Context(), false)
}

func TestWarmSessionPoolSerialisesSameConversation(t *testing.T) {
	pool := newWarmSessionPool(10, 24*time.Hour)
	first, _, err := pool.acquire(t.Context(), "conversation", "config", func(context.Context) (agent.WarmHost, error) {
		return &fakeWarmHost{id: "one"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	acquired := make(chan *warmSessionLease, 1)
	go func() {
		lease, _, acquireErr := pool.acquire(t.Context(), "conversation", "config", func(context.Context) (agent.WarmHost, error) {
			t.Error("same conversation unexpectedly created a second host")
			return &fakeWarmHost{id: "two"}, nil
		})
		if acquireErr != nil {
			t.Errorf("second acquire: %v", acquireErr)
			return
		}
		acquired <- lease
	}()

	select {
	case <-acquired:
		t.Fatal("second acquire completed before the first lease released")
	case <-time.After(20 * time.Millisecond):
	}
	_ = first.Release(t.Context(), true)
	select {
	case lease := <-acquired:
		_ = lease.Release(t.Context(), false)
	case <-time.After(time.Second):
		t.Fatal("second acquire did not wake after release")
	}
}

func TestWarmSessionPoolStrictCapWaitsWhenAllBusy(t *testing.T) {
	pool := newWarmSessionPool(1, 24*time.Hour)
	var creates atomic.Int32
	create := func(context.Context) (agent.WarmHost, error) {
		id := creates.Add(1)
		return &fakeWarmHost{id: string(rune('0' + id))}, nil
	}
	first, _, err := pool.acquire(t.Context(), "first", "config", create)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if _, _, err := pool.acquire(ctx, "second", "config", create); err == nil {
		t.Fatal("acquire unexpectedly exceeded strict capacity")
	}
	if got := creates.Load(); got != 1 {
		t.Fatalf("creates = %d, want 1", got)
	}
	_ = first.Release(t.Context(), false)
}

func TestWarmSessionPoolEvictsLRUIdleHost(t *testing.T) {
	pool := newWarmSessionPool(2, 24*time.Hour)
	now := time.Unix(100, 0)
	pool.now = func() time.Time { return now }
	hosts := map[string]*fakeWarmHost{}
	create := func(key string) func(context.Context) (agent.WarmHost, error) {
		return func(context.Context) (agent.WarmHost, error) {
			h := &fakeWarmHost{id: key}
			hosts[key] = h
			return h, nil
		}
	}

	first, _, _ := pool.acquire(t.Context(), "first", "config", create("first"))
	_ = first.Release(t.Context(), true)
	now = now.Add(time.Minute)
	second, _, _ := pool.acquire(t.Context(), "second", "config", create("second"))
	_ = second.Release(t.Context(), true)
	now = now.Add(time.Minute)
	third, hit, err := pool.acquire(t.Context(), "third", "config", create("third"))
	if err != nil || hit {
		t.Fatalf("third acquire = hit %v, err %v", hit, err)
	}
	if hosts["first"].closed.Load() != 1 || hosts["second"].closed.Load() != 0 {
		t.Fatalf("wrong LRU victim: first closed=%d, second closed=%d", hosts["first"].closed.Load(), hosts["second"].closed.Load())
	}
	_ = third.Release(t.Context(), false)
	_ = pool.close(t.Context())
}

func TestWarmSessionPoolExpiresIdleButNotBusy(t *testing.T) {
	pool := newWarmSessionPool(2, time.Hour)
	now := time.Unix(100, 0)
	pool.now = func() time.Time { return now }
	idleHost := &fakeWarmHost{id: "idle"}
	busyHost := &fakeWarmHost{id: "busy"}
	idle, _, _ := pool.acquire(t.Context(), "idle", "config", func(context.Context) (agent.WarmHost, error) { return idleHost, nil })
	_ = idle.Release(t.Context(), true)
	busy, _, _ := pool.acquire(t.Context(), "busy", "config", func(context.Context) (agent.WarmHost, error) { return busyHost, nil })

	now = now.Add(time.Hour)
	if err := pool.sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if idleHost.closed.Load() != 1 || busyHost.closed.Load() != 0 {
		t.Fatalf("idle closed=%d, busy closed=%d", idleHost.closed.Load(), busyHost.closed.Load())
	}
	_ = busy.Release(t.Context(), false)
}

func TestWarmSessionPoolConfigChangeReplacesIdleHost(t *testing.T) {
	pool := newWarmSessionPool(1, 24*time.Hour)
	oldHost := &fakeWarmHost{id: "old"}
	old, _, _ := pool.acquire(t.Context(), "conversation", "old-config", func(context.Context) (agent.WarmHost, error) { return oldHost, nil })
	_ = old.Release(t.Context(), true)
	newHost := &fakeWarmHost{id: "new"}
	replacement, hit, err := pool.acquire(t.Context(), "conversation", "new-config", func(context.Context) (agent.WarmHost, error) { return newHost, nil })
	if err != nil || hit {
		t.Fatalf("replacement acquire = hit %v, err %v", hit, err)
	}
	if oldHost.closed.Load() != 1 || replacement.Host() != newHost {
		t.Fatal("configuration change did not replace the old host")
	}
	_ = replacement.Release(t.Context(), false)
}

func TestWarmSessionPoolFactoryReservationIsStrict(t *testing.T) {
	pool := newWarmSessionPool(1, 24*time.Hour)
	started := make(chan struct{})
	unblock := make(chan struct{})
	var creates atomic.Int32
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		lease, _, err := pool.acquire(t.Context(), "first", "config", func(context.Context) (agent.WarmHost, error) {
			creates.Add(1)
			close(started)
			<-unblock
			return &fakeWarmHost{id: "first"}, nil
		})
		if err == nil {
			_ = lease.Release(t.Context(), false)
		}
	}()
	<-started
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	_, _, _ = pool.acquire(ctx, "second", "config", func(context.Context) (agent.WarmHost, error) {
		creates.Add(1)
		return &fakeWarmHost{id: "second"}, nil
	})
	if got := creates.Load(); got != 1 {
		t.Fatalf("factories ran concurrently past the cap: %d", got)
	}
	close(unblock)
	wg.Wait()
}

func TestWarmSessionPoolConcurrentCloseStopsActiveLease(t *testing.T) {
	pool := newWarmSessionPool(1, time.Hour)
	host := &fakeWarmHost{id: "one"}
	lease, _, err := pool.acquire(t.Context(), "conversation", "config", func(context.Context) (agent.WarmHost, error) {
		return host, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 2)
	go func() { closed <- pool.close(t.Context()) }()
	go func() { closed <- pool.close(t.Context()) }()
	for i := 0; i < 2; i++ {
		select {
		case err := <-closed:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("concurrent close did not finish after lease release")
		}
	}
	if host.closed.Load() != 1 {
		t.Fatalf("active host close calls = %d, want 1", host.closed.Load())
	}
	if err := lease.Release(t.Context(), true); err != nil {
		t.Fatalf("late release after shutdown: %v", err)
	}
}
