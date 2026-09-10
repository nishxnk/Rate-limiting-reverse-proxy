package metrics

import (
	"sync"
	"sync/atomic"
)

// Subscriber is one connected Server-Sent Events client.
type Subscriber struct {
	ch      chan []byte
	dropped atomic.Uint64
}

// Events is the channel of encoded frames to write to the client. It is closed
// when the hub shuts down.
func (s *Subscriber) Events() <-chan []byte { return s.ch }

// Dropped counts frames skipped because this client could not keep up.
func (s *Subscriber) Dropped() uint64 { return s.dropped.Load() }

// Hub fans one broadcast out to every connected dashboard.
//
// Sends are non-blocking: a slow client loses frames rather than stalling the
// broadcaster, which keeps a hung browser tab from backing up the whole server.
type Hub struct {
	mu     sync.RWMutex
	subs   map[*Subscriber]struct{}
	closed bool
	buffer int
}

// NewHub returns an empty hub. buffer is the per-subscriber queue depth.
func NewHub(buffer int) *Hub {
	if buffer < 1 {
		buffer = 8
	}
	return &Hub{subs: make(map[*Subscriber]struct{}), buffer: buffer}
}

// Subscribe registers a new client. The returned Subscriber must be passed to
// Unsubscribe when the connection ends. It is nil only if the hub is closed.
func (h *Hub) Subscribe() *Subscriber {
	s := &Subscriber{ch: make(chan []byte, h.buffer)}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		close(s.ch)
		return s
	}
	h.subs[s] = struct{}{}
	return s
}

// Unsubscribe removes a client and closes its channel exactly once.
func (h *Hub) Unsubscribe(s *Subscriber) {
	if s == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs[s]; !ok {
		return
	}
	delete(h.subs, s)
	close(s.ch)
}

// Broadcast delivers frame to every subscriber that has room for it.
func (h *Hub) Broadcast(frame []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for s := range h.subs {
		select {
		case s.ch <- frame:
		default:
			s.dropped.Add(1)
		}
	}
}

// Len reports the number of connected clients.
func (h *Hub) Len() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs)
}

// Close disconnects every subscriber. Further Subscribe calls return a closed
// subscriber so handlers exit immediately.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for s := range h.subs {
		delete(h.subs, s)
		close(s.ch)
	}
}
