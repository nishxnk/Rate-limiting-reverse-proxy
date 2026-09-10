package metrics

import (
	"sync"
	"testing"
	"time"
)

func newTestCollector(windowSeconds int) (*Collector, func(time.Duration)) {
	c := NewCollector(windowSeconds, time.Minute)
	var mu sync.Mutex
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	c.start = now
	c.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	return c, func(d time.Duration) {
		mu.Lock()
		now = now.Add(d)
		mu.Unlock()
	}
}

func TestCollectorCountsOutcomes(t *testing.T) {
	c, _ := newTestCollector(60)

	c.Record(Event{Key: "ip:1.1.1.1", Kind: "ip", Allowed: true, Status: 200, Limit: 5, Remaining: 4, Latency: 2 * time.Millisecond})
	c.Record(Event{Key: "ip:1.1.1.1", Kind: "ip", Allowed: false, Status: 429, Limit: 5, Latency: time.Millisecond})
	c.Record(Event{Key: "ip:2.2.2.2", Kind: "ip", Allowed: true, Status: 503, Limit: 5, Remaining: 4, Latency: 3 * time.Millisecond})

	snap := c.Snapshot(10)
	if snap.Total != 3 || snap.Allowed != 2 || snap.Blocked != 1 || snap.Errors != 1 {
		t.Errorf("snapshot = %+v, want 3 total, 2 allowed, 1 blocked, 1 error", snap)
	}
	if want := 1.0 / 3.0; snap.BlockRate < want-0.001 || snap.BlockRate > want+0.001 {
		t.Errorf("block rate = %v, want about %v", snap.BlockRate, want)
	}
	if snap.AvgLatencyMS < 1.9 || snap.AvgLatencyMS > 2.1 {
		t.Errorf("average latency = %v ms, want about 2", snap.AvgLatencyMS)
	}
	if snap.TrackedClients != 2 || len(snap.Clients) != 2 {
		t.Errorf("clients = %d tracked with %d rows, want 2 and 2", snap.TrackedClients, len(snap.Clients))
	}
	if snap.Clients[0].Key != "ip:1.1.1.1" {
		t.Errorf("busiest client = %q, want ip:1.1.1.1 first", snap.Clients[0].Key)
	}
}

func TestSeriesIsZeroFilledAndOrdered(t *testing.T) {
	c, advance := newTestCollector(10)

	c.Record(Event{Key: "k", Allowed: true, Status: 200})
	advance(3 * time.Second)
	c.Record(Event{Key: "k", Allowed: false, Status: 429})

	series := c.Series(c.now())
	if len(series) != 10 {
		t.Fatalf("series length = %d, want 10", len(series))
	}
	for i := 1; i < len(series); i++ {
		if series[i].TS != series[i-1].TS+1 {
			t.Fatalf("series is not contiguous at index %d: %d then %d", i, series[i-1].TS, series[i].TS)
		}
	}
	last := series[len(series)-1]
	if last.Blocked != 1 || last.Allowed != 0 {
		t.Errorf("newest bucket = %+v, want the blocked request only", last)
	}
	older := series[len(series)-4]
	if older.Allowed != 1 {
		t.Errorf("bucket three seconds back = %+v, want the allowed request", older)
	}
}

// TestSeriesForgetsAWholeLapOfTheRing guards the ring buffer indexing: a bucket
// from a previous lap must never be reported as current data.
func TestSeriesForgetsAWholeLapOfTheRing(t *testing.T) {
	c, advance := newTestCollector(10)
	c.Record(Event{Key: "k", Allowed: true, Status: 200})

	advance(10 * time.Second)
	for _, b := range c.Series(c.now()) {
		if b.Total != 0 {
			t.Fatalf("bucket %+v still carries data a full lap later", b)
		}
	}
}

func TestSweepForgetsIdleClients(t *testing.T) {
	c, advance := newTestCollector(60)
	c.Record(Event{Key: "ip:1.1.1.1", Kind: "ip", Allowed: true, Status: 200})

	advance(30 * time.Second)
	if n := c.Sweep(); n != 0 {
		t.Errorf("swept %d clients, want 0 before the TTL elapses", n)
	}

	advance(2 * time.Minute)
	if n := c.Sweep(); n != 1 {
		t.Errorf("swept %d clients, want 1 after the TTL elapses", n)
	}
	if _, total := c.Clients(c.now(), 10); total != 0 {
		t.Errorf("tracked clients = %d, want 0", total)
	}
	// Totals survive: sweeping is about the table, not the counters.
	if snap := c.Snapshot(10); snap.Total != 1 {
		t.Errorf("total = %d, want the lifetime counter preserved", snap.Total)
	}
}

func TestClientTableStaysBounded(t *testing.T) {
	c, _ := newTestCollector(60)
	c.maxClients = 20

	for i := 0; i < 400; i++ {
		c.Record(Event{Key: "ip:10.0.0." + string(rune('a'+i%26)) + string(rune('a'+i/26)), Kind: "ip", Allowed: true, Status: 200})
	}
	_, total := c.Clients(c.now(), 500)
	if total > c.maxClients+64 {
		t.Errorf("tracked clients = %d, want the table capped near %d", total, c.maxClients)
	}
}

func TestConcurrentRecordIsSafe(t *testing.T) {
	c, _ := newTestCollector(60)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				c.Record(Event{Key: "shared", Allowed: j%2 == 0, Status: 200, Latency: time.Millisecond})
			}
		}(i)
	}
	wg.Wait()

	snap := c.Snapshot(10)
	if snap.Total != 10000 || snap.Allowed != 5000 || snap.Blocked != 5000 {
		t.Errorf("snapshot = %+v, want 10000 total split evenly", snap)
	}
}

func TestHubBroadcastsAndDropsSlowSubscribers(t *testing.T) {
	h := NewHub(2)
	sub := h.Subscribe()
	if h.Len() != 1 {
		t.Fatalf("subscribers = %d, want 1", h.Len())
	}

	h.Broadcast([]byte("one"))
	if got := string(<-sub.Events()); got != "one" {
		t.Errorf("received %q, want one", got)
	}

	// Fill the buffer and then overflow it: the extra frame is dropped rather
	// than blocking the broadcaster.
	h.Broadcast([]byte("a"))
	h.Broadcast([]byte("b"))
	h.Broadcast([]byte("c"))
	if sub.Dropped() != 1 {
		t.Errorf("dropped = %d, want 1", sub.Dropped())
	}

	h.Unsubscribe(sub)
	if h.Len() != 0 {
		t.Errorf("subscribers = %d, want 0 after unsubscribing", h.Len())
	}
	h.Unsubscribe(sub) // must be idempotent

	// The channel is closed, so ranging over it drains what was buffered and
	// then stops rather than blocking forever.
	drained := 0
	for range sub.Events() {
		drained++
	}
	if drained != 2 {
		t.Errorf("drained %d buffered frames, want 2", drained)
	}
}

func TestHubCloseDisconnectsEveryone(t *testing.T) {
	h := NewHub(1)
	a, b := h.Subscribe(), h.Subscribe()
	h.Close()

	for i, sub := range []*Subscriber{a, b} {
		if _, open := <-sub.Events(); open {
			t.Errorf("subscriber %d is still connected after Close", i)
		}
	}
	// Subscribing after Close hands back a closed channel so handlers exit.
	late := h.Subscribe()
	if _, open := <-late.Events(); open {
		t.Error("a subscription made after Close is still open")
	}
	h.Close() // must be idempotent
}
