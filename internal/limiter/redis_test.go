package limiter

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// redisFixture wires a RedisLimiter to a backing server.
//
// By default that server is an in-process miniredis, which executes the same
// Lua script and sorted-set commands, so the distributed path is covered on
// every run. Point TEST_REDIS_ADDR at a real server to exercise these tests
// against production Redis instead.
type redisFixture struct {
	limiter *RedisLimiter
	client  redis.UniversalClient
	mini    *miniredis.Miniredis // nil when a real server is used
}

// advance moves the clock forward. With miniredis time is simulated, so the
// tests stay fast and deterministic; against a real server it sleeps.
func (f *redisFixture) advance(d time.Duration) {
	if f.mini != nil {
		f.mini.FastForward(d)
		return
	}
	time.Sleep(d)
}

func newRedisFixture(t *testing.T) *redisFixture {
	t.Helper()
	prefix := fmt.Sprintf("test:%d:", time.Now().UnixNano())

	if addr := os.Getenv("TEST_REDIS_ADDR"); addr != "" {
		client := redis.NewClient(&redis.Options{Addr: addr, DialTimeout: 2 * time.Second})
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := client.Ping(ctx).Err()
		cancel()
		if err != nil {
			_ = client.Close()
			t.Skipf("TEST_REDIS_ADDR=%s is not reachable: %v", addr, err)
		}
		f := &redisFixture{limiter: NewRedisLimiter(client, prefix), client: client}
		t.Cleanup(func() {
			cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
			if _, err := f.limiter.Reset(cctx); err != nil {
				t.Logf("cleaning up test keys: %v", err)
			}
			ccancel()
			_ = client.Close()
		})
		return f
	}

	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return &redisFixture{limiter: NewRedisLimiter(client, prefix), client: client, mini: mini}
}

func TestRedisLimiterEnforcesTheWindow(t *testing.T) {
	f := newRedisFixture(t)
	p := Policy{Limit: 5, Window: 2 * time.Second}
	ctx := context.Background()

	for i := 1; i <= p.Limit; i++ {
		d, err := f.limiter.Allow(ctx, "client", p)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		if !d.Allowed {
			t.Fatalf("request %d: denied, want allowed", i)
		}
		if got, want := d.Remaining, p.Limit-i; got != want {
			t.Errorf("request %d: remaining = %d, want %d", i, got, want)
		}
		if d.Backend != BackendRedis {
			t.Errorf("request %d: backend = %q, want %q", i, d.Backend, BackendRedis)
		}
	}

	d, err := f.limiter.Allow(ctx, "client", p)
	if err != nil {
		t.Fatalf("overflow request: %v", err)
	}
	if d.Allowed {
		t.Fatal("request over the limit was allowed")
	}
	if d.RetryAfter <= 0 || d.RetryAfter > p.Window {
		t.Errorf("retry after = %v, want a value in (0, %v]", d.RetryAfter, p.Window)
	}

	// Once the whole window has slid past, the budget is back.
	f.advance(p.Window + 100*time.Millisecond)
	if d, err := f.limiter.Allow(ctx, "client", p); err != nil || !d.Allowed {
		t.Fatalf("after the window expired: allowed = %v, err = %v, want allowed", d.Allowed, err)
	}
}

func TestRedisLimiterKeysAreIndependent(t *testing.T) {
	f := newRedisFixture(t)
	p := Policy{Limit: 1, Window: time.Minute}
	ctx := context.Background()

	if d, err := f.limiter.Allow(ctx, "alice", p); err != nil || !d.Allowed {
		t.Fatalf("alice first request: allowed = %v, err = %v", d.Allowed, err)
	}
	if d, err := f.limiter.Allow(ctx, "alice", p); err != nil || d.Allowed {
		t.Fatalf("alice second request: allowed = %v, err = %v, want denied", d.Allowed, err)
	}
	if d, err := f.limiter.Allow(ctx, "bob", p); err != nil || !d.Allowed {
		t.Fatalf("bob first request: allowed = %v, err = %v, want allowed", d.Allowed, err)
	}
}

// TestRedisLimiterIsAtomicUnderConcurrency is the reason the decision lives in
// a Lua script: a read-then-write implementation would let more than the limit
// through when many requests arrive at once.
func TestRedisLimiterIsAtomicUnderConcurrency(t *testing.T) {
	f := newRedisFixture(t)
	const (
		limit   = 25
		callers = 200
	)
	p := Policy{Limit: limit, Window: time.Minute}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		allowed int
		start   = make(chan struct{})
	)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			d, err := f.limiter.Allow(context.Background(), "hot", p)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if d.Allowed {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if allowed != limit {
		t.Errorf("allowed %d of %d concurrent requests, want exactly %d", allowed, callers, limit)
	}
}

func TestRedisLimiterPeekAndActiveKeys(t *testing.T) {
	f := newRedisFixture(t)
	p := Policy{Limit: 10, Window: time.Minute}
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := f.limiter.Allow(ctx, "watched", p); err != nil {
			t.Fatalf("Allow: %v", err)
		}
	}

	used, err := f.limiter.Peek(ctx, "watched", p)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if used != 3 {
		t.Errorf("Peek used = %d, want 3", used)
	}

	keys, err := f.limiter.ActiveKeys(ctx, 50)
	if err != nil {
		t.Fatalf("ActiveKeys: %v", err)
	}
	if keys["watched"] != 3 {
		t.Errorf("ActiveKeys[watched] = %d, want 3 (all keys: %v)", keys["watched"], keys)
	}

	deleted, err := f.limiter.Reset(ctx)
	if err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if deleted != 1 {
		t.Errorf("Reset deleted %d keys, want 1", deleted)
	}
	if after, err := f.limiter.ActiveKeys(ctx, 50); err != nil || len(after) != 0 {
		t.Errorf("after Reset: keys = %v, err = %v, want none", after, err)
	}
}

func TestManagerPrefersRedisWhenHealthy(t *testing.T) {
	f := newRedisFixture(t)
	mgr := NewManager(f.limiter, NewMemoryLimiter(0), ManagerConfig{
		Timeout:       2 * time.Second,
		ProbeInterval: time.Hour,
		Policy:        Policy{Limit: 3, Window: time.Minute},
		Logger:        quietLogger(),
	})
	t.Cleanup(func() { _ = mgr.Memory().Close() })

	d := mgr.Allow(context.Background(), "client")
	if d.Backend != BackendRedis {
		t.Fatalf("backend = %q, want %q while Redis is healthy", d.Backend, BackendRedis)
	}
	st := mgr.Stats()
	if st.Degraded {
		t.Error("Stats().Degraded = true, want false")
	}
	if st.RedisServed == 0 {
		t.Error("Stats().RedisServed = 0, want Redis credited with the decision")
	}
}

func TestPolicyStoreRoundTripAndBroadcast(t *testing.T) {
	f := newRedisFixture(t)
	store := NewPolicyStore(f.client, fmt.Sprintf("test:%d:", time.Now().UnixNano()), quietLogger())
	ctx := context.Background()

	if _, ok, err := store.Load(ctx); err != nil || ok {
		t.Fatalf("Load on an empty store = (ok %v, err %v), want (false, nil)", ok, err)
	}

	want := Policy{Limit: 42, Window: 5 * time.Second}
	if err := store.Save(ctx, want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, ok, err := store.Load(ctx)
	if err != nil || !ok {
		t.Fatalf("Load = (ok %v, err %v), want (true, nil)", ok, err)
	}
	if !got.Equal(want) {
		t.Errorf("Load = %+v, want %+v", got, want)
	}
}
