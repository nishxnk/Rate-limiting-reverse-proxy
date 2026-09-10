package limiter

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// slidingWindowScript evaluates the whole decision inside Redis so that
// concurrent requests for the same key cannot race between the read and the
// write.
//
//	KEYS[1] - the sorted set holding one member per request in the window
//	ARGV[1] - now, in milliseconds since the epoch (supplied by the caller so
//	          the script stays deterministic and replica-safe)
//	ARGV[2] - window width in milliseconds
//	ARGV[3] - request limit inside the window
//	ARGV[4] - unique member id for this request
//
// Returns {allowed, used, reset_ms}.
const slidingWindowScript = `
local key       = KEYS[1]
local now_ms    = tonumber(ARGV[1])
local window_ms = tonumber(ARGV[2])
local limit     = tonumber(ARGV[3])
local member    = ARGV[4]

-- Drop everything that has slid out of the window.
redis.call('ZREMRANGEBYSCORE', key, '-inf', now_ms - window_ms)

local used = redis.call('ZCARD', key)
local allowed = 0
if used < limit then
  redis.call('ZADD', key, now_ms, member)
  used = used + 1
  allowed = 1
end

-- Keep the key alive only for as long as it can hold live entries.
redis.call('PEXPIRE', key, window_ms)

-- The window frees a slot when its oldest member expires.
local reset_ms = window_ms
local oldest = redis.call('ZRANGE', key, 0, 0, 'WITHSCORES')
if oldest[2] then
  reset_ms = tonumber(oldest[2]) + window_ms - now_ms
  if reset_ms < 0 then reset_ms = 0 end
end

return {allowed, used, math.floor(reset_ms)}
`

// RedisLimiter is a distributed sliding-window counter built on a Redis sorted
// set. Every instance of the proxy sharing a Redis deployment enforces one
// global limit per client key.
type RedisLimiter struct {
	client redis.UniversalClient
	script *redis.Script
	prefix string

	instance string
	seq      atomic.Uint64
	owned    bool // true when this limiter created the client and must close it
}

var _ Limiter = (*RedisLimiter)(nil)

// NewRedisLimiter wraps an existing client. prefix is prepended to every key.
func NewRedisLimiter(client redis.UniversalClient, prefix string) *RedisLimiter {
	return &RedisLimiter{
		client:   client,
		script:   redis.NewScript(slidingWindowScript),
		prefix:   prefix,
		instance: newInstanceID(),
	}
}

// Name implements Limiter.
func (r *RedisLimiter) Name() string { return BackendRedis }

// Client exposes the underlying client for health checks and policy storage.
func (r *RedisLimiter) Client() redis.UniversalClient { return r.client }

// Allow implements Limiter. A returned error always means "backend unusable":
// the caller is expected to fall back rather than to fail the request.
func (r *RedisLimiter) Allow(ctx context.Context, key string, p Policy) (Decision, error) {
	p = p.Normalize()
	now := time.Now()
	windowMS := p.Window.Milliseconds()
	if windowMS < 1 {
		windowMS = 1
	}

	raw, err := r.script.Run(ctx, r.client,
		[]string{r.prefix + key},
		now.UnixMilli(), windowMS, p.Limit, r.member(now),
	).Slice()
	if err != nil {
		return Decision{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if len(raw) != 3 {
		return Decision{}, fmt.Errorf("%w: unexpected script reply %v", ErrUnavailable, raw)
	}

	allowed, err1 := toInt(raw[0])
	used, err2 := toInt(raw[1])
	resetMS, err3 := toInt(raw[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return Decision{}, fmt.Errorf("%w: malformed script reply %v", ErrUnavailable, raw)
	}

	resetAt := now.Add(time.Duration(resetMS) * time.Millisecond)
	if allowed == 0 {
		return deny(BackendRedis, p, used, resetAt, now), nil
	}
	remaining := p.Limit - used
	if remaining < 0 {
		remaining = 0
	}
	return Decision{
		Allowed:    true,
		Limit:      p.Limit,
		Window:     p.Window,
		Used:       used,
		Remaining:  remaining,
		ResetAt:    resetAt,
		RetryAfter: time.Duration(resetMS) * time.Millisecond,
		Backend:    BackendRedis,
	}, nil
}

// Peek reports the usage recorded for key without counting a request.
func (r *RedisLimiter) Peek(ctx context.Context, key string, p Policy) (int, error) {
	p = p.Normalize()
	cutoff := strconv.FormatInt(time.Now().Add(-p.Window).UnixMilli(), 10)
	n, err := r.client.ZCount(ctx, r.prefix+key, "("+cutoff, "+inf").Result()
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return int(n), nil
}

// ActiveKeys scans Redis for keys currently tracked by any instance and returns
// their usage. It is bounded by maxKeys so the dashboard can never trigger an
// expensive full keyspace walk.
func (r *RedisLimiter) ActiveKeys(ctx context.Context, maxKeys int) (map[string]int, error) {
	if maxKeys <= 0 {
		maxKeys = 100
	}
	out := make(map[string]int, maxKeys)
	var cursor uint64
	for len(out) < maxKeys {
		keys, next, err := r.client.Scan(ctx, cursor, r.prefix+"*", 100).Result()
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		if len(keys) > 0 {
			pipe := r.client.Pipeline()
			cmds := make([]*redis.IntCmd, 0, len(keys))
			for _, k := range keys {
				cmds = append(cmds, pipe.ZCard(ctx, k))
			}
			if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
				return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
			}
			for i, k := range keys {
				n := cmds[i].Val()
				if n == 0 {
					continue
				}
				out[strings.TrimPrefix(k, r.prefix)] = int(n)
				if len(out) >= maxKeys {
					break
				}
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return out, nil
}

// Ping checks connectivity to Redis.
func (r *RedisLimiter) Ping(ctx context.Context) error {
	if err := r.client.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return nil
}

// Reset deletes every rate-limit key managed by this prefix.
func (r *RedisLimiter) Reset(ctx context.Context) (int, error) {
	var cursor uint64
	deleted := 0
	for {
		keys, next, err := r.client.Scan(ctx, cursor, r.prefix+"*", 200).Result()
		if err != nil {
			return deleted, fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		if len(keys) > 0 {
			n, err := r.client.Del(ctx, keys...).Result()
			if err != nil {
				return deleted, fmt.Errorf("%w: %v", ErrUnavailable, err)
			}
			deleted += int(n)
		}
		cursor = next
		if cursor == 0 {
			return deleted, nil
		}
	}
}

// Close releases the client when this limiter owns it.
func (r *RedisLimiter) Close() error {
	if r.owned {
		return r.client.Close()
	}
	return nil
}

// member builds a value that is unique across instances, goroutines and
// requests landing in the same millisecond.
func (r *RedisLimiter) member(now time.Time) string {
	var b strings.Builder
	b.Grow(len(r.instance) + 30)
	b.WriteString(r.instance)
	b.WriteByte('-')
	b.WriteString(strconv.FormatInt(now.UnixNano(), 36))
	b.WriteByte('-')
	b.WriteString(strconv.FormatUint(r.seq.Add(1), 36))
	return b.String()
}

func toInt(v any) (int, error) {
	switch n := v.(type) {
	case int64:
		return int(n), nil
	case int:
		return n, nil
	case float64:
		return int(n), nil
	case string:
		return strconv.Atoi(n)
	default:
		return 0, fmt.Errorf("unsupported reply type %T", v)
	}
}

func newInstanceID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is not fatal here: uniqueness is only needed to
		// avoid sorted-set member collisions, and the nanosecond timestamp plus
		// sequence number already carries most of the entropy.
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}
