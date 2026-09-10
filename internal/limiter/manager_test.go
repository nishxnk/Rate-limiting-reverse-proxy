package limiter

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// deadAddr returns an address nothing is listening on, so every connection is
// refused immediately. That is a faithful stand-in for a Redis outage without
// waiting on a dial timeout.
func deadAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("closing the reserved port: %v", err)
	}
	return addr
}

func newOfflineRedisManager(t *testing.T, p Policy, threshold int) *Manager {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{
		Addr:         deadAddr(t),
		DialTimeout:  100 * time.Millisecond,
		ReadTimeout:  100 * time.Millisecond,
		WriteTimeout: 100 * time.Millisecond,
		MaxRetries:   -1,
	})
	mgr := NewManager(NewRedisLimiter(rdb, "test:"), NewMemoryLimiter(0), ManagerConfig{
		Timeout:          100 * time.Millisecond,
		FailureThreshold: threshold,
		Cooldown:         2 * time.Second,
		ProbeInterval:    time.Hour, // the tests drive the breaker, not the prober
		Policy:           p,
		Logger:           quietLogger(),
	})
	t.Cleanup(func() {
		if err := mgr.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return mgr
}

func TestManagerFallsBackToMemoryWhenRedisIsOffline(t *testing.T) {
	p := Policy{Limit: 2, Window: time.Minute}
	mgr := newOfflineRedisManager(t, p, 3)
	ctx := context.Background()

	// The fallback must still enforce the policy, not wave traffic through.
	for i := 1; i <= p.Limit; i++ {
		d := mgr.Allow(ctx, "client")
		if !d.Allowed {
			t.Fatalf("request %d: denied, want allowed", i)
		}
		if d.Backend != BackendMemory {
			t.Fatalf("request %d: backend = %q, want %q", i, d.Backend, BackendMemory)
		}
	}
	if d := mgr.Allow(ctx, "client"); d.Allowed {
		t.Fatal("request over the limit was allowed by the fallback")
	}

	st := mgr.Stats()
	if !st.Degraded {
		t.Error("Stats().Degraded = false, want true while Redis is offline")
	}
	if st.FallbackServed == 0 {
		t.Error("Stats().FallbackServed = 0, want the fallback to be credited")
	}
	if st.LastError == "" {
		t.Error("Stats().LastError is empty, want the Redis failure recorded")
	}
}

func TestManagerCircuitStopsCallingOfflineRedis(t *testing.T) {
	const threshold = 2
	mgr := newOfflineRedisManager(t, Policy{Limit: 1000, Window: time.Minute}, threshold)
	ctx := context.Background()

	for i := 0; i < threshold; i++ {
		mgr.Allow(ctx, "client")
	}
	st := mgr.Stats()
	if st.ConsecutiveFailures < threshold {
		t.Fatalf("consecutive failures = %d, want at least %d", st.ConsecutiveFailures, threshold)
	}
	if st.CircuitOpenUntil.IsZero() {
		t.Fatal("circuit did not open after reaching the failure threshold")
	}

	// With the circuit open the Redis call is skipped entirely, so the decision
	// comes back far faster than the configured backend timeout.
	start := time.Now()
	d := mgr.Allow(ctx, "client")
	elapsed := time.Since(start)
	if d.Backend != BackendMemory {
		t.Errorf("backend = %q, want %q", d.Backend, BackendMemory)
	}
	if elapsed > 20*time.Millisecond {
		t.Errorf("decision took %v with the circuit open, want it to short-circuit", elapsed)
	}
}

func TestManagerDoesNotTripBreakerWhenCallerCancels(t *testing.T) {
	mgr := newOfflineRedisManager(t, Policy{Limit: 5, Window: time.Minute}, 2)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	d := mgr.Allow(ctx, "client")
	if !d.Allowed {
		t.Error("a cancelled caller should still receive a fallback decision")
	}
	if got := mgr.Stats().ConsecutiveFailures; got != 0 {
		t.Errorf("consecutive failures = %d, want 0: caller cancellation is not a backend fault", got)
	}
}

func TestManagerWithoutRedisUsesMemoryOnly(t *testing.T) {
	mgr := NewManager(nil, NewMemoryLimiter(0), ManagerConfig{
		Policy: Policy{Limit: 1, Window: time.Minute},
		Logger: quietLogger(),
	})
	t.Cleanup(func() {
		if err := mgr.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	if mgr.RedisEnabled() {
		t.Error("RedisEnabled() = true, want false")
	}
	if d := mgr.Allow(context.Background(), "client"); !d.Allowed || d.Backend != BackendMemory {
		t.Errorf("first decision = %+v, want allowed from memory", d)
	}
	if d := mgr.Allow(context.Background(), "client"); d.Allowed {
		t.Error("second request exceeded the limit but was allowed")
	}
	if st := mgr.Stats(); st.RedisEnabled || st.Backend != BackendMemory {
		t.Errorf("stats = %+v, want redis disabled and the memory backend", st)
	}
}

func TestManagerSetPolicyTakesEffectImmediately(t *testing.T) {
	mgr := NewManager(nil, NewMemoryLimiter(0), ManagerConfig{
		Policy: Policy{Limit: 1, Window: time.Minute},
		Logger: quietLogger(),
	})
	t.Cleanup(func() { _ = mgr.Close() })
	ctx := context.Background()

	if d := mgr.Allow(ctx, "client"); !d.Allowed {
		t.Fatal("first request denied under a limit of 1")
	}
	if d := mgr.Allow(ctx, "client"); d.Allowed {
		t.Fatal("second request allowed under a limit of 1")
	}

	applied := mgr.SetPolicy(Policy{Limit: 5, Window: time.Minute})
	if applied.Limit != 5 {
		t.Fatalf("applied limit = %d, want 5", applied.Limit)
	}
	d := mgr.Allow(ctx, "client")
	if !d.Allowed {
		t.Error("request denied after the limit was raised")
	}
	if d.Limit != 5 {
		t.Errorf("decision limit = %d, want 5", d.Limit)
	}
}
