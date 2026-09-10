// Package metrics collects the live traffic counters that back the dashboard:
// a rolling per-second time series, aggregate totals, and a table of the client
// keys currently being tracked.
//
// Everything here is safe for concurrent use from every request goroutine.
package metrics

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Bucket is one second of the rolling traffic series.
type Bucket struct {
	TS      int64  `json:"ts"` // unix seconds
	Total   uint64 `json:"total"`
	Allowed uint64 `json:"allowed"`
	Blocked uint64 `json:"blocked"`
	Errors  uint64 `json:"errors"`
}

// ClientView is one row of the dashboard client table.
type ClientView struct {
	Key       string  `json:"key"`
	Kind      string  `json:"kind"` // "ip" or "api_key"
	Requests  uint64  `json:"requests"`
	Allowed   uint64  `json:"allowed"`
	Blocked   uint64  `json:"blocked"`
	Limit     int     `json:"limit"`
	Remaining int     `json:"remaining"`
	ResetInMS int64   `json:"reset_in_ms"`
	Backend   string  `json:"backend"`
	LastSeen  int64   `json:"last_seen_ms"`
	IdleMS    int64   `json:"idle_ms"`
	AvgMS     float64 `json:"avg_latency_ms"`
}

// Event describes one completed request.
type Event struct {
	Key       string
	Kind      string
	Allowed   bool
	Status    int
	Limit     int
	Remaining int
	ResetAt   time.Time
	Backend   string
	// Latency is the time the proxy spent on the request. Callers are expected
	// to always measure it; a zero value is treated as a sample below the
	// clock resolution rather than as a missing measurement.
	Latency time.Duration
}

type clientStat struct {
	mu        sync.Mutex
	kind      string
	requests  uint64
	allowed   uint64
	blocked   uint64
	limit     int
	remaining int
	resetAt   time.Time
	backend   string
	lastSeen  time.Time
	latSumNS  int64
	latCount  int64
}

// Collector aggregates request outcomes.
type Collector struct {
	start time.Time
	now   func() time.Time

	total   atomic.Uint64
	allowed atomic.Uint64
	blocked atomic.Uint64
	errors  atomic.Uint64

	latSumNS atomic.Int64
	latCount atomic.Int64

	mu     sync.Mutex
	series []Bucket // ring indexed by unix second modulo len

	clients    sync.Map // string -> *clientStat
	clientTTL  time.Duration
	maxClients int
}

// NewCollector returns a collector retaining windowSeconds of per-second
// history. Client rows are forgotten after they have been idle for ttl.
func NewCollector(windowSeconds int, ttl time.Duration) *Collector {
	if windowSeconds < 10 {
		windowSeconds = 10
	}
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &Collector{
		start:      time.Now(),
		now:        time.Now,
		series:     make([]Bucket, windowSeconds),
		clientTTL:  ttl,
		maxClients: 500,
	}
}

// Record folds one request outcome into every aggregate.
func (c *Collector) Record(ev Event) {
	now := c.now()

	c.total.Add(1)
	isError := ev.Status >= 500
	if ev.Allowed {
		c.allowed.Add(1)
	} else {
		c.blocked.Add(1)
	}
	if isError {
		c.errors.Add(1)
	}
	// Every request is a latency sample, including one that finished faster
	// than the platform clock can resolve (on Windows time.Now tops out near
	// half a millisecond). Dropping those would bias the mean upwards and, on
	// a fast local upstream, leave it undefined.
	if ev.Latency > 0 {
		c.latSumNS.Add(int64(ev.Latency))
	}
	c.latCount.Add(1)

	c.recordSeries(now, ev.Allowed, isError)

	if ev.Key != "" {
		c.recordClient(now, ev)
	}
}

func (c *Collector) recordSeries(now time.Time, allowed, isError bool) {
	sec := now.Unix()
	idx := int(sec % int64(len(c.series)))

	c.mu.Lock()
	b := &c.series[idx]
	if b.TS != sec {
		*b = Bucket{TS: sec}
	}
	b.Total++
	if allowed {
		b.Allowed++
	} else {
		b.Blocked++
	}
	if isError {
		b.Errors++
	}
	c.mu.Unlock()
}

func (c *Collector) recordClient(now time.Time, ev Event) {
	v, loaded := c.clients.LoadOrStore(ev.Key, &clientStat{kind: ev.Kind})
	if !loaded {
		c.enforceClientCap()
	}
	s := v.(*clientStat)

	s.mu.Lock()
	s.kind = ev.Kind
	s.requests++
	if ev.Allowed {
		s.allowed++
	} else {
		s.blocked++
	}
	s.limit = ev.Limit
	s.remaining = ev.Remaining
	s.resetAt = ev.ResetAt
	s.backend = ev.Backend
	s.lastSeen = now
	if ev.Latency > 0 {
		s.latSumNS += int64(ev.Latency)
	}
	s.latCount++
	s.mu.Unlock()
}

// enforceClientCap keeps the tracked client table bounded. It only runs when a
// brand new key appears, so the scan is rare.
func (c *Collector) enforceClientCap() {
	n := 0
	c.clients.Range(func(_, _ any) bool { n++; return n <= c.maxClients+64 })
	if n <= c.maxClients+64 {
		return
	}
	// Over budget: drop the least recently seen keys.
	type aged struct {
		key  any
		seen time.Time
	}
	all := make([]aged, 0, n)
	c.clients.Range(func(k, v any) bool {
		s := v.(*clientStat)
		s.mu.Lock()
		seen := s.lastSeen
		s.mu.Unlock()
		all = append(all, aged{k, seen})
		return true
	})
	sort.Slice(all, func(i, j int) bool { return all[i].seen.Before(all[j].seen) })
	for i := 0; i < len(all)-c.maxClients; i++ {
		c.clients.Delete(all[i].key)
	}
}

// Snapshot is the payload rendered by the dashboard and streamed over SSE.
type Snapshot struct {
	Total          uint64       `json:"total"`
	Allowed        uint64       `json:"allowed"`
	Blocked        uint64       `json:"blocked"`
	Errors         uint64       `json:"errors"`
	BlockRate      float64      `json:"block_rate"`
	RPS            float64      `json:"rps"`
	AvgLatencyMS   float64      `json:"avg_latency_ms"`
	UptimeSeconds  float64      `json:"uptime_seconds"`
	TrackedClients int          `json:"tracked_clients"`
	Series         []Bucket     `json:"series"`
	Clients        []ClientView `json:"clients"`
}

// Snapshot builds a consistent view of all counters. maxClients caps the number
// of client rows returned; pass 0 for the default of 50.
func (c *Collector) Snapshot(maxClients int) Snapshot {
	if maxClients <= 0 {
		maxClients = 50
	}
	now := c.now()
	total := c.total.Load()
	blocked := c.blocked.Load()

	var blockRate float64
	if total > 0 {
		blockRate = float64(blocked) / float64(total)
	}
	var avgMS float64
	if n := c.latCount.Load(); n > 0 {
		avgMS = float64(c.latSumNS.Load()) / float64(n) / float64(time.Millisecond)
	}

	series := c.Series(now)
	// The last complete second is the best instantaneous rate estimate.
	var rps float64
	if len(series) >= 2 {
		rps = float64(series[len(series)-2].Total)
	}

	clients, tracked := c.Clients(now, maxClients)

	return Snapshot{
		Total:          total,
		Allowed:        c.allowed.Load(),
		Blocked:        blocked,
		Errors:         c.errors.Load(),
		BlockRate:      blockRate,
		RPS:            rps,
		AvgLatencyMS:   avgMS,
		UptimeSeconds:  now.Sub(c.start).Seconds(),
		TrackedClients: tracked,
		Series:         series,
		Clients:        clients,
	}
}

// Series returns the rolling window ordered oldest to newest, with quiet
// seconds zero-filled so the graph keeps advancing when there is no traffic.
func (c *Collector) Series(now time.Time) []Bucket {
	n := int64(len(c.series))
	end := now.Unix()
	out := make([]Bucket, 0, n)

	c.mu.Lock()
	for i := n - 1; i >= 0; i-- {
		sec := end - i
		b := c.series[((sec%n)+n)%n]
		if b.TS != sec {
			b = Bucket{TS: sec}
		}
		out = append(out, b)
	}
	c.mu.Unlock()
	return out
}

// Clients returns up to limit client rows, busiest first, plus the total number
// of keys currently tracked.
func (c *Collector) Clients(now time.Time, limit int) ([]ClientView, int) {
	rows := make([]ClientView, 0, 64)
	c.clients.Range(func(k, v any) bool {
		s := v.(*clientStat)
		s.mu.Lock()
		var avg float64
		if s.latCount > 0 {
			avg = float64(s.latSumNS) / float64(s.latCount) / float64(time.Millisecond)
		}
		var resetIn int64
		if d := s.resetAt.Sub(now); d > 0 {
			resetIn = d.Milliseconds()
		}
		row := ClientView{
			Key:       k.(string),
			Kind:      s.kind,
			Requests:  s.requests,
			Allowed:   s.allowed,
			Blocked:   s.blocked,
			Limit:     s.limit,
			Remaining: s.remaining,
			ResetInMS: resetIn,
			Backend:   s.backend,
			LastSeen:  s.lastSeen.UnixMilli(),
			IdleMS:    now.Sub(s.lastSeen).Milliseconds(),
			AvgMS:     avg,
		}
		s.mu.Unlock()
		rows = append(rows, row)
		return true
	})

	total := len(rows)
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Requests != rows[j].Requests {
			return rows[i].Requests > rows[j].Requests
		}
		return rows[i].Key < rows[j].Key
	})
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, total
}

// Sweep forgets clients idle for longer than the configured TTL.
func (c *Collector) Sweep() int {
	cutoff := c.now().Add(-c.clientTTL)
	removed := 0
	c.clients.Range(func(k, v any) bool {
		s := v.(*clientStat)
		s.mu.Lock()
		stale := s.lastSeen.Before(cutoff)
		s.mu.Unlock()
		if stale {
			c.clients.Delete(k)
			removed++
		}
		return true
	})
	return removed
}

// ResetClients clears the tracked client table without touching the totals.
func (c *Collector) ResetClients() {
	c.clients.Range(func(k, _ any) bool {
		c.clients.Delete(k)
		return true
	})
}
