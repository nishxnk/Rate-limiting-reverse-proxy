package limiter

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// ManagerConfig tunes the failover behaviour between Redis and the local
// limiter.
type ManagerConfig struct {
	// Timeout bounds every individual Redis call. Exceeding it counts as a
	// backend failure and the request is served by the fallback.
	Timeout time.Duration
	// FailureThreshold is how many consecutive Redis failures trip the circuit.
	FailureThreshold int
	// Cooldown is how long the circuit stays open before a probe is allowed.
	Cooldown time.Duration
	// ProbeInterval is how often the background health check pings Redis.
	ProbeInterval time.Duration
	// Policy is the initial rate limit.
	Policy Policy
	// Logger receives failover and recovery events. Defaults to slog.Default().
	Logger *slog.Logger
}

func (c ManagerConfig) withDefaults() ManagerConfig {
	if c.Timeout <= 0 {
		c.Timeout = 200 * time.Millisecond
	}
	if c.FailureThreshold < 1 {
		c.FailureThreshold = 3
	}
	if c.Cooldown <= 0 {
		c.Cooldown = 5 * time.Second
	}
	if c.ProbeInterval <= 0 {
		c.ProbeInterval = 2 * time.Second
	}
	c.Policy = c.Policy.Normalize()
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return c
}

// Manager routes every decision to Redis first and degrades to the in-memory
// limiter when Redis is unreachable, slow, or has failed repeatedly.
//
// A circuit breaker keeps a Redis outage from adding the full timeout to every
// request: after FailureThreshold consecutive failures the primary is skipped
// entirely until a background probe finds it healthy again.
type Manager struct {
	primary  *RedisLimiter // nil when Redis is not configured
	fallback *MemoryLimiter
	cfg      ManagerConfig

	policy atomic.Pointer[Policy]

	mu        sync.Mutex
	failures  int
	openUntil time.Time
	lastErr   string

	redisHealthy atomic.Bool
	degraded     atomic.Bool

	redisServed    atomic.Uint64
	fallbackServed atomic.Uint64

	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// NewManager builds a Manager. primary may be nil, in which case every request
// is served by the in-memory limiter.
func NewManager(primary *RedisLimiter, fallback *MemoryLimiter, cfg ManagerConfig) *Manager {
	cfg = cfg.withDefaults()
	m := &Manager{
		primary:  primary,
		fallback: fallback,
		cfg:      cfg,
		stop:     make(chan struct{}),
	}
	p := cfg.Policy
	m.policy.Store(&p)
	m.redisHealthy.Store(primary != nil)
	// Degraded means "Redis was wanted but cannot be used". Running without
	// Redis on purpose is a supported mode, not a degradation.
	m.degraded.Store(false)

	if primary != nil {
		m.wg.Add(1)
		go m.probeLoop()
	}
	return m
}

// Policy returns the rate limit currently in force.
func (m *Manager) Policy() Policy { return *m.policy.Load() }

// SetPolicy replaces the rate limit for all subsequent requests and returns the
// normalized value that was stored.
func (m *Manager) SetPolicy(p Policy) Policy {
	p = p.Normalize()
	m.policy.Store(&p)
	return p
}

// Allow counts one request for key under the current policy. It never fails:
// when every backend is unhappy the in-memory limiter still produces a
// decision, so the caller always has an answer.
func (m *Manager) Allow(ctx context.Context, key string) Decision {
	return m.AllowPolicy(ctx, key, m.Policy())
}

// AllowPolicy is Allow with an explicit policy override.
func (m *Manager) AllowPolicy(ctx context.Context, key string, p Policy) Decision {
	if m.primary != nil && m.tryPrimary() {
		rctx, cancel := context.WithTimeout(ctx, m.cfg.Timeout)
		d, err := m.primary.Allow(rctx, key, p)
		cancel()
		switch {
		case err == nil:
			m.recordSuccess()
			m.redisServed.Add(1)
			return d
		case ctx.Err() != nil:
			// The calling context died (client disconnected, or the server is
			// shutting down). That is not a Redis fault, so leave the breaker
			// alone and let the fallback answer.
		default:
			m.recordFailure(err)
		}
	}
	d, _ := m.fallback.Allow(ctx, key, p)
	m.fallbackServed.Add(1)
	return d
}

// Degraded reports whether decisions are currently coming from the local
// fallback instead of Redis.
func (m *Manager) Degraded() bool { return m.degraded.Load() }

// RedisEnabled reports whether a Redis backend was configured at all.
func (m *Manager) RedisEnabled() bool { return m.primary != nil }

// Redis exposes the primary limiter (nil when Redis is disabled).
func (m *Manager) Redis() *RedisLimiter { return m.primary }

// Memory exposes the fallback limiter.
func (m *Manager) Memory() *MemoryLimiter { return m.fallback }

// Stats is a point-in-time view of the failover state, surfaced on the
// dashboard and the health endpoint.
type Stats struct {
	RedisEnabled        bool      `json:"redis_enabled"`
	RedisHealthy        bool      `json:"redis_healthy"`
	Degraded            bool      `json:"degraded"`
	Backend             string    `json:"backend"`
	RedisServed         uint64    `json:"redis_served"`
	FallbackServed      uint64    `json:"fallback_served"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
	CircuitOpenUntil    time.Time `json:"circuit_open_until"`
	TrackedKeys         int       `json:"tracked_keys"`
	Evictions           int64     `json:"evictions"`
	LastError           string    `json:"last_error,omitempty"`
}

// Stats snapshots the current failover state.
func (m *Manager) Stats() Stats {
	m.mu.Lock()
	failures, openUntil, lastErr := m.failures, m.openUntil, m.lastErr
	m.mu.Unlock()

	backend := BackendRedis
	if m.primary == nil || m.degraded.Load() {
		backend = BackendMemory
	}
	return Stats{
		RedisEnabled:        m.primary != nil,
		RedisHealthy:        m.redisHealthy.Load(),
		Degraded:            m.degraded.Load(),
		Backend:             backend,
		RedisServed:         m.redisServed.Load(),
		FallbackServed:      m.fallbackServed.Load(),
		ConsecutiveFailures: failures,
		CircuitOpenUntil:    openUntil,
		TrackedKeys:         m.fallback.Keys(),
		Evictions:           m.fallback.Evictions(),
		LastError:           lastErr,
	}
}

// Close stops the health prober and releases both limiters.
func (m *Manager) Close() error {
	m.stopOnce.Do(func() { close(m.stop) })
	m.wg.Wait()
	var err error
	if m.primary != nil {
		err = m.primary.Close()
	}
	if ferr := m.fallback.Close(); err == nil {
		err = ferr
	}
	return err
}

// tryPrimary reports whether the circuit permits a Redis call. When the
// cooldown has elapsed exactly one request is let through as a probe.
func (m *Manager) tryPrimary() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.openUntil.IsZero() {
		return true
	}
	now := time.Now()
	if now.After(m.openUntil) {
		// Half-open: re-arm the window so only this request probes, and let a
		// success clear the breaker.
		m.openUntil = now.Add(m.cfg.Cooldown)
		return true
	}
	return false
}

func (m *Manager) recordFailure(err error) {
	m.mu.Lock()
	m.failures++
	m.lastErr = err.Error()
	tripped := false
	if m.failures >= m.cfg.FailureThreshold && m.openUntil.IsZero() {
		m.openUntil = time.Now().Add(m.cfg.Cooldown)
		tripped = true
	}
	failures := m.failures
	m.mu.Unlock()

	m.redisHealthy.Store(false)
	if !m.degraded.Swap(true) || tripped {
		m.cfg.Logger.Warn("rate limiter failing over to in-memory backend",
			"error", err, "consecutive_failures", failures, "cooldown", m.cfg.Cooldown)
	}
}

func (m *Manager) recordSuccess() {
	m.mu.Lock()
	recovered := m.failures > 0 || !m.openUntil.IsZero()
	m.failures = 0
	m.openUntil = time.Time{}
	if recovered {
		m.lastErr = ""
	}
	m.mu.Unlock()

	m.redisHealthy.Store(true)
	if m.degraded.Swap(false) && recovered {
		m.cfg.Logger.Info("rate limiter recovered, serving from redis")
	}
}

// probeLoop keeps the Redis health flag fresh and closes the circuit as soon as
// the backend answers again, so recovery does not have to wait for traffic.
func (m *Manager) probeLoop() {
	defer m.wg.Done()
	t := time.NewTicker(m.cfg.ProbeInterval)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), m.cfg.Timeout)
			err := m.primary.Ping(ctx)
			cancel()
			if err != nil {
				m.redisHealthy.Store(false)
				m.mu.Lock()
				m.lastErr = err.Error()
				m.mu.Unlock()
				continue
			}
			m.redisHealthy.Store(true)
			m.mu.Lock()
			wasOpen := !m.openUntil.IsZero()
			m.failures = 0
			m.openUntil = time.Time{}
			if wasOpen {
				m.lastErr = ""
			}
			m.mu.Unlock()
			if m.degraded.Swap(false) {
				m.cfg.Logger.Info("redis reachable again, closing fallback circuit")
			}
		}
	}
}
