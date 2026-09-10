package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// StartBroadcaster pushes a stats frame to every connected dashboard on each
// tick, and periodically forgets idle clients so the table stays current. It
// blocks until ctx is cancelled, so run it in its own goroutine.
func (s *Server) StartBroadcaster(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	sweep := time.NewTicker(30 * time.Second)
	defer sweep.Stop()

	for {
		select {
		case <-ctx.Done():
			s.hub.Close()
			return
		case <-sweep.C:
			if n := s.metrics.Sweep(); n > 0 {
				s.log.Debug("swept idle clients from the tracking table", "removed", n)
			}
		case <-ticker.C:
			// Serialising once for all subscribers keeps a hundred open
			// dashboards from costing a hundred marshals per second.
			frame, err := encodeFrame("stats", s.Snapshot())
			if err != nil {
				s.log.Error("could not encode metrics frame", "error", err)
				continue
			}
			s.hub.Broadcast(frame)
		}
	}
}

// encodeFrame renders one Server-Sent Event.
func encodeFrame(event string, payload any) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	buf.Grow(len(body) + 32)
	buf.WriteString("event: ")
	buf.WriteString(event)
	buf.WriteString("\ndata: ")
	// A JSON document never contains a raw newline, so one data line is enough.
	buf.Write(body)
	buf.WriteString("\n\n")
	return buf.Bytes(), nil
}

// handleStream serves the live metrics stream over Server-Sent Events.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	// Tell nginx and friends not to buffer the stream.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	rc := http.NewResponseController(w)
	// A stream outlives any per-request deadline. Clearing both matters: the
	// server keeps a background read running to notice the client going away,
	// and an expired read deadline would cancel the request context.
	_ = rc.SetWriteDeadline(time.Time{})
	_ = rc.SetReadDeadline(time.Time{})

	sub := s.hub.Subscribe()
	defer s.hub.Unsubscribe(sub)

	// Send the current state immediately so the page renders without waiting
	// for the first tick.
	if frame, err := encodeFrame("stats", s.Snapshot()); err == nil {
		if _, err := w.Write(frame); err != nil {
			return
		}
		if err := rc.Flush(); err != nil {
			s.log.Debug("sse client does not support flushing", "error", err)
			return
		}
	}

	// Comment lines keep proxies and browsers from timing the stream out when
	// the broadcaster is idle.
	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-keepalive.C:
			if _, err := w.Write([]byte(": keepalive\n\n")); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		case frame, open := <-sub.Events():
			if !open {
				return
			}
			if _, err := w.Write(frame); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}
	}
}
