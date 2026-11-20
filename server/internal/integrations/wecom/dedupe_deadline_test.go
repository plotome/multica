package wecom

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// Stall just this client's settle command until its existing budget expires.
// Unlike pausing the Redis server, this cannot perturb other package fixtures.
type settleDeadlineHook struct {
	stall bool
	t     *testing.T
}

func (*settleDeadlineHook) DialHook(next redis.DialHook) redis.DialHook {
	return func(ctx context.Context, network, addr string) (net.Conn, error) { return next(ctx, network, addr) }
}
func (h *settleDeadlineHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		start := time.Now()
		if h.stall && cmd.Name() == "evalsha" && cmd.Args()[1] == redisSettle.Hash() {
			<-ctx.Done()
			h.t.Logf("controlled scheduling stall: %s elapsed=%s error=%v", cmd.Name(), time.Since(start), ctx.Err())
			return ctx.Err()
		}
		err := next(ctx, cmd)
		h.t.Logf("Redis command=%s elapsed=%s error=%v", cmd.Name(), time.Since(start), err)
		return err
	}
}
func (*settleDeadlineHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func TestRedisDedupe_DeadlineDoesNotChangeForeignOwner(t *testing.T) {
	rdb := wecomTestRedis(t)
	h := &settleDeadlineHook{t: t}
	rdb.AddHook(h)
	ctx := context.Background()
	const key, a, b = "wecom:outbound:deadline", "owner-a/ev", "owner-b/ev"
	if err := rdb.Set(ctx, key, b, time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	d := NewRedisDedupe(rdb, testClaimBudget, slog.Default())
	h.stall = true
	if ok, err := d.Settle(ctx, key, a); ok || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline-expired settle = %t, %v", ok, err)
	}
	h.stall = false
	if owner, err := rdb.Get(ctx, key).Result(); err != nil || owner != b {
		t.Fatalf("foreign owner changed: %q, %v", owner, err)
	}
	if ok, err := d.Settle(ctx, key, a); ok || err != nil {
		t.Fatalf("foreign settle after stall = %t, %v", ok, err)
	}
	if ok, err := d.Settle(ctx, key, b); !ok || err != nil {
		t.Fatalf("owner settle after stall = %t, %v", ok, err)
	}
}
