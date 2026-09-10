// Package limiter implements the distributed sliding-window rate limiter and
// its process-local fallback.
//
// Two independent implementations satisfy the Limiter interface:
//
//	RedisLimiter  - a sliding-window counter backed by a Redis sorted set,
//	                evaluated atomically inside a Lua script.
//	MemoryLimiter - a sliding-window log kept in a sync.Map, used when Redis
//	                is unreachable or slow.
//
// Manager composes the two behind a circuit breaker so a Redis outage degrades
// the service instead of breaking it.
package limiter

import (
	"context"
	"errors"
	"time"
)

// Backend names reported on every Decision.
const (
	BackendRedis  = "redis"
	BackendMemory = "memory"
)

// ErrUnavailable is returned by a Limiter that cannot reach its backing store.
var ErrUnavailable = errors.New("limiter: backend unavailable")

// Policy is the rate limit applied to a single client key.
type Policy struct {
	// Limit is the maximum number of requests allowed inside Window.
	Limit int
	// Window is the width of the sliding window.
	Window time.Duration
}

// Normalize clamps a policy to values the limiters can actually enforce.
func (p Policy) Normalize() Policy {
	if p.Limit < 1 {
		p.Limit = 1
	}
	if p.Limit > 1_000_000 {
		p.Limit = 1_000_000
	}
	if p.Window < time.Millisecond {
		p.Window = time.Second
	}
	if p.Window > 24*time.Hour {
		p.Window = 24 * time.Hour
	}
	return p
}

// Equal reports whether two policies describe the same limit.
func (p Policy) Equal(o Policy) bool { return p.Limit == o.Limit && p.Window == o.Window }

// Decision is the outcome of a single Allow call.
type Decision struct {
	// Allowed reports whether the request may proceed.
	Allowed bool
	// Limit is the policy limit that was applied.
	Limit int
	// Window is the width of the sliding window that was applied.
	Window time.Duration
	// Used is the number of requests counted inside the current window,
	// including this one when it was allowed.
	Used int
	// Remaining is the number of further requests permitted right now.
	Remaining int
	// ResetAt is the instant at which the window frees up at least one slot.
	ResetAt time.Time
	// RetryAfter is ResetAt expressed as a delay from now.
	RetryAfter time.Duration
	// Backend records which limiter produced this decision.
	Backend string
}

// Limiter counts a request against a client key and decides its fate.
type Limiter interface {
	// Allow registers one request for key and returns the decision. A non-nil
	// error means the decision is not trustworthy and the caller should fall
	// back to another limiter.
	Allow(ctx context.Context, key string, p Policy) (Decision, error)
	// Name identifies the backend for logging and metrics.
	Name() string
	// Close releases any resources held by the limiter.
	Close() error
}

// deny builds the Decision for a request that exceeded its policy.
func deny(backend string, p Policy, used int, resetAt time.Time, now time.Time) Decision {
	retry := resetAt.Sub(now)
	if retry < 0 {
		retry = 0
	}
	return Decision{
		Allowed:    false,
		Limit:      p.Limit,
		Window:     p.Window,
		Used:       used,
		Remaining:  0,
		ResetAt:    resetAt,
		RetryAfter: retry,
		Backend:    backend,
	}
}
