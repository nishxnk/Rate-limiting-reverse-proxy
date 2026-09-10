package limiter

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// memEntry holds the sliding-window log for one client key.
//
// hits stores the nanosecond timestamps of the requests still inside the
// window, in ascending order. Because a timestamp is only appended when the
// request is allowed, len(hits) never exceeds the policy limit and memory per
// key stays bounded.
type memEntry struct {
	mu       sync.Mutex
	hits     []int64
	window   int64 // width of the last applied window, in nanoseconds
	dead     bool  // set by the janitor just before the entry is evicted
	lastSeen atomic.Int64
}

// prune drops every hit at or before cutoff. hits is sorted, so a single scan
// from the front is enough.
func (e *memEntry) prune(cutoff int64) {
	i := 0
	for i < len(e.hits) && e.hits[i] <= cutoff {
		i++
	}
	if i == 0 {
		return
	}
	e.hits = append(e.hits[:0], e.hits[i:]...)
	// Release the backing array once it is mostly empty so a burst does not
	// pin memory forever.
	if cap(e.hits) > 64 && cap(e.hits) > 4*len(e.hits) {
		shrunk := make([]int64, len(e.hits), max(len(e.hits)*2, 8))
		copy(shrunk, e.hits)
		e.hits = shrunk
	}
}

// MemoryLimiter is a process-local sliding-window limiter backed by a sync.Map.
// It is the fallback used when the Redis backend is unavailable: each instance
// then enforces the policy independently, which is approximate across a fleet
// but never lets traffic through unmetered.
//
// A janitor goroutine evicts keys that have been idle for twice their window so
// that a large, churning key space does not leak memory.
type MemoryLimiter struct {
	entries sync.Map // string -> *memEntry

	now      func() time.Time
	interval time.Duration
	minTTL   time.Duration

	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup

	evictions atomic.Int64
}

// compile-time interface check.
var _ Limiter = (*MemoryLimiter)(nil)

// NewMemoryLimiter returns a running MemoryLimiter. Call Close to stop the
// janitor goroutine. A non-positive interval disables background cleanup
// (entries are still pruned lazily on access).
func NewMemoryLimiter(interval time.Duration) *MemoryLimiter {
	m := &MemoryLimiter{
		now:      time.Now,
		interval: interval,
		minTTL:   time.Minute,
		stop:     make(chan struct{}),
	}
	if interval > 0 {
		m.wg.Add(1)
		go m.janitor()
	}
	return m
}

// Name implements Limiter.
func (m *MemoryLimiter) Name() string { return BackendMemory }

// Allow implements Limiter. It never returns an error: the in-memory limiter is
// the last line of defence and must always produce a decision.
func (m *MemoryLimiter) Allow(_ context.Context, key string, p Policy) (Decision, error) {
	p = p.Normalize()
	now := m.now()
	nowNS := now.UnixNano()
	cutoff := nowNS - int64(p.Window)

	// The janitor may evict an entry between the lookup and the lock. When that
	// happens the entry is flagged dead and we retry with a fresh one.
	for attempt := 0; ; attempt++ {
		v, _ := m.entries.LoadOrStore(key, &memEntry{})
		e := v.(*memEntry)
		e.lastSeen.Store(nowNS)

		e.mu.Lock()
		if e.dead {
			e.mu.Unlock()
			m.entries.CompareAndDelete(key, e)
			if attempt < 8 {
				runtime.Gosched()
				continue
			}
			// Pathological contention: fall through with a detached entry
			// rather than spinning forever. At worst one request is not
			// counted against a key that has been idle for minutes.
			e = &memEntry{}
			e.mu.Lock()
		}

		e.window = int64(p.Window)
		e.prune(cutoff)
		used := len(e.hits)

		if used >= p.Limit {
			resetAt := time.Unix(0, e.hits[0]+int64(p.Window))
			e.mu.Unlock()
			return deny(BackendMemory, p, used, resetAt, now), nil
		}

		e.hits = append(e.hits, nowNS)
		used++
		oldest := e.hits[0]
		e.mu.Unlock()

		resetAt := time.Unix(0, oldest+int64(p.Window))
		retry := resetAt.Sub(now)
		if retry < 0 {
			retry = 0
		}
		return Decision{
			Allowed:    true,
			Limit:      p.Limit,
			Window:     p.Window,
			Used:       used,
			Remaining:  p.Limit - used,
			ResetAt:    resetAt,
			RetryAfter: retry,
			Backend:    BackendMemory,
		}, nil
	}
}

// Peek reports the current usage for key without counting a request.
func (m *MemoryLimiter) Peek(key string, p Policy) (used int, ok bool) {
	p = p.Normalize()
	v, found := m.entries.Load(key)
	if !found {
		return 0, false
	}
	e := v.(*memEntry)
	cutoff := m.now().UnixNano() - int64(p.Window)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.prune(cutoff)
	return len(e.hits), true
}

// Keys returns the number of client keys currently tracked.
func (m *MemoryLimiter) Keys() int {
	n := 0
	m.entries.Range(func(_, _ any) bool { n++; return true })
	return n
}

// Evictions returns how many idle keys the janitor has reclaimed.
func (m *MemoryLimiter) Evictions() int64 { return m.evictions.Load() }

// Reset drops all tracked state. Used by tests and by the dashboard's
// "clear clients" action.
func (m *MemoryLimiter) Reset() {
	m.entries.Range(func(k, v any) bool {
		e := v.(*memEntry)
		e.mu.Lock()
		e.dead = true
		e.mu.Unlock()
		m.entries.CompareAndDelete(k, e)
		return true
	})
}

// Close stops the janitor. It is safe to call more than once.
func (m *MemoryLimiter) Close() error {
	m.stopOnce.Do(func() { close(m.stop) })
	m.wg.Wait()
	return nil
}

func (m *MemoryLimiter) janitor() {
	defer m.wg.Done()
	t := time.NewTicker(m.interval)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-t.C:
			m.cleanup()
		}
	}
}

// cleanup evicts entries idle for longer than twice their window (and at least
// minTTL), and prunes expired hits from the ones that survive.
func (m *MemoryLimiter) cleanup() {
	nowNS := m.now().UnixNano()
	m.entries.Range(func(k, v any) bool {
		e := v.(*memEntry)
		e.mu.Lock()
		ttl := 2 * e.window
		if minTTL := int64(m.minTTL); ttl < minTTL {
			ttl = minTTL
		}
		idle := nowNS-e.lastSeen.Load() > ttl
		if idle {
			e.dead = true
			e.hits = nil
			e.mu.Unlock()
			if m.entries.CompareAndDelete(k, e) {
				m.evictions.Add(1)
			}
			return true
		}
		e.prune(nowNS - e.window)
		e.mu.Unlock()
		return true
	})
}
