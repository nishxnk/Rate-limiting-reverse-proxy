package limiter

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fixedClock lets a test drive the limiter without sleeping.
type fixedClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fixedClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fixedClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newTestMemoryLimiter(t *testing.T) (*MemoryLimiter, *fixedClock) {
	t.Helper()
	clock := &fixedClock{t: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)}
	m := NewMemoryLimiter(0) // no janitor: the tests drive cleanup explicitly
	m.now = clock.now
	t.Cleanup(func() {
		if err := m.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return m, clock
}

func TestMemoryLimiterAllowsUpToLimitThenDenies(t *testing.T) {
	m, _ := newTestMemoryLimiter(t)
	p := Policy{Limit: 3, Window: time.Second}
	ctx := context.Background()

	for i := 1; i <= p.Limit; i++ {
		d, err := m.Allow(ctx, "client", p)
		if err != nil {
			t.Fatalf("request %d: unexpected error: %v", i, err)
		}
		if !d.Allowed {
			t.Fatalf("request %d: want allowed, got denied", i)
		}
		if got, want := d.Remaining, p.Limit-i; got != want {
			t.Errorf("request %d: remaining = %d, want %d", i, got, want)
		}
		if d.Backend != BackendMemory {
			t.Errorf("request %d: backend = %q, want %q", i, d.Backend, BackendMemory)
		}
	}

	d, err := m.Allow(ctx, "client", p)
	if err != nil {
		t.Fatalf("overflow request: unexpected error: %v", err)
	}
	if d.Allowed {
		t.Fatal("request over the limit was allowed")
	}
	if d.Remaining != 0 {
		t.Errorf("remaining = %d, want 0", d.Remaining)
	}
	if d.RetryAfter <= 0 || d.RetryAfter > p.Window {
		t.Errorf("retry after = %v, want a value in (0, %v]", d.RetryAfter, p.Window)
	}
}

func TestMemoryLimiterWindowSlides(t *testing.T) {
	m, clock := newTestMemoryLimiter(t)
	p := Policy{Limit: 2, Window: time.Second}

	mustAllow(t, m, "client", p, true)
	clock.advance(400 * time.Millisecond)
	mustAllow(t, m, "client", p, true)
	mustAllow(t, m, "client", p, false)

	// The first hit ages out 1s after it was recorded, freeing exactly one slot.
	clock.advance(600 * time.Millisecond)
	mustAllow(t, m, "client", p, true)
	mustAllow(t, m, "client", p, false)

	// After a full idle window every slot is back.
	clock.advance(2 * time.Second)
	mustAllow(t, m, "client", p, true)
	mustAllow(t, m, "client", p, true)
	mustAllow(t, m, "client", p, false)
}

func TestMemoryLimiterKeysAreIndependent(t *testing.T) {
	m, _ := newTestMemoryLimiter(t)
	p := Policy{Limit: 1, Window: time.Second}

	mustAllow(t, m, "alice", p, true)
	mustAllow(t, m, "alice", p, false)
	mustAllow(t, m, "bob", p, true)

	if got := m.Keys(); got != 2 {
		t.Errorf("tracked keys = %d, want 2", got)
	}
}

func TestMemoryLimiterCleanupEvictsIdleKeys(t *testing.T) {
	m, clock := newTestMemoryLimiter(t)
	p := Policy{Limit: 5, Window: time.Second}

	mustAllow(t, m, "idle", p, true)
	if got := m.Keys(); got != 1 {
		t.Fatalf("tracked keys = %d, want 1", got)
	}

	// Not yet past the eviction TTL (max of 2 windows and one minute).
	clock.advance(30 * time.Second)
	m.cleanup()
	if got := m.Keys(); got != 1 {
		t.Errorf("key evicted too early: tracked keys = %d, want 1", got)
	}

	clock.advance(2 * time.Minute)
	m.cleanup()
	if got := m.Keys(); got != 0 {
		t.Errorf("tracked keys = %d, want 0 after eviction", got)
	}
	if got := m.Evictions(); got != 1 {
		t.Errorf("evictions = %d, want 1", got)
	}

	// A previously evicted key must start over cleanly.
	mustAllow(t, m, "idle", p, true)
}

func TestMemoryLimiterPeekDoesNotConsume(t *testing.T) {
	m, _ := newTestMemoryLimiter(t)
	p := Policy{Limit: 2, Window: time.Second}

	if _, ok := m.Peek("ghost", p); ok {
		t.Error("Peek reported an unknown key as tracked")
	}
	mustAllow(t, m, "client", p, true)

	used, ok := m.Peek("client", p)
	if !ok || used != 1 {
		t.Fatalf("Peek = (%d, %v), want (1, true)", used, ok)
	}
	// Peeking twice must not have consumed the remaining slot.
	mustAllow(t, m, "client", p, true)
	mustAllow(t, m, "client", p, false)
}

func TestMemoryLimiterConcurrentAllowRespectsLimit(t *testing.T) {
	m, _ := newTestMemoryLimiter(t)
	const (
		limit   = 50
		callers = 400
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
			d, err := m.Allow(context.Background(), "hot", p)
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

func TestPolicyNormalizeClampsGarbage(t *testing.T) {
	got := Policy{Limit: 0, Window: 0}.Normalize()
	if got.Limit != 1 {
		t.Errorf("limit = %d, want 1", got.Limit)
	}
	if got.Window != time.Second {
		t.Errorf("window = %v, want 1s", got.Window)
	}
	if got := (Policy{Limit: 5_000_000, Window: 48 * time.Hour}).Normalize(); got.Limit != 1_000_000 || got.Window != 24*time.Hour {
		t.Errorf("normalize = %+v, want limit 1000000 and window 24h", got)
	}
}

func mustAllow(t *testing.T, m *MemoryLimiter, key string, p Policy, want bool) {
	t.Helper()
	d, err := m.Allow(context.Background(), key, p)
	if err != nil {
		t.Fatalf("Allow(%q): unexpected error: %v", key, err)
	}
	if d.Allowed != want {
		t.Fatalf("Allow(%q).Allowed = %v, want %v", key, d.Allowed, want)
	}
}
