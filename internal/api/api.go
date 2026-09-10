// Package api exposes the control plane consumed by the admin dashboard:
// live metrics, health, the tracked client table, and dynamic policy updates.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/avd450/ratelimit-proxy/internal/limiter"
	"github.com/avd450/ratelimit-proxy/internal/metrics"
	"github.com/avd450/ratelimit-proxy/internal/proxy"
)

// Options configures the API server.
type Options struct {
	Limiter  *limiter.Manager
	Metrics  *metrics.Collector
	Hub      *metrics.Hub
	Store    *limiter.PolicyStore // optional: shares policy changes across instances
	Upstream *proxy.Upstream
	// Proxy, when set, lets the control plane change the upstream target at
	// runtime. UpstreamStore (optional) shares that change across the fleet.
	Proxy            *proxy.Handler
	UpstreamStore    *limiter.UpstreamStore
	UpstreamTimeout  time.Duration
	UpstreamEditable bool
	Logger           *slog.Logger
	Version          string
}

// Server implements the control-plane HTTP handlers.
type Server struct {
	limiter          *limiter.Manager
	metrics          *metrics.Collector
	hub              *metrics.Hub
	store            *limiter.PolicyStore
	upstream         *proxy.Upstream
	proxy            *proxy.Handler
	upstreamStore    *limiter.UpstreamStore
	upstreamTimeout  time.Duration
	upstreamEditable bool
	log              *slog.Logger
	version          string
	started          time.Time
}

// New builds the control plane. Limiter and Metrics are required.
func New(o Options) (*Server, error) {
	if o.Limiter == nil {
		return nil, errors.New("api: limiter is required")
	}
	if o.Metrics == nil {
		return nil, errors.New("api: metrics collector is required")
	}
	if o.Hub == nil {
		o.Hub = metrics.NewHub(8)
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Version == "" {
		o.Version = "dev"
	}
	return &Server{
		limiter:          o.Limiter,
		metrics:          o.Metrics,
		hub:              o.Hub,
		store:            o.Store,
		upstream:         o.Upstream,
		proxy:            o.Proxy,
		upstreamStore:    o.UpstreamStore,
		upstreamTimeout:  o.UpstreamTimeout,
		upstreamEditable: o.UpstreamEditable,
		log:              o.Logger,
		version:          o.Version,
		started:          time.Now(),
	}, nil
}

// Register wires the control-plane routes onto mux under prefix.
//
// Everything the proxy owns lives under the prefix so that a proxied
// application keeps its own /api and /dashboard. Pass "" to mount at the root.
func (s *Server) Register(mux *http.ServeMux, prefix string) {
	p := strings.TrimSuffix(prefix, "/")
	mux.HandleFunc("GET "+p+"/api/health", s.handleHealth)
	mux.HandleFunc("GET "+p+"/api/stats", s.handleStats)
	mux.HandleFunc("GET "+p+"/api/stream", s.handleStream)
	mux.HandleFunc("GET "+p+"/api/policy", s.handlePolicyGet)
	mux.HandleFunc("PUT "+p+"/api/policy", s.handlePolicySet)
	mux.HandleFunc("POST "+p+"/api/policy", s.handlePolicySet)
	mux.HandleFunc("GET "+p+"/api/clients", s.handleClients)
	mux.HandleFunc("POST "+p+"/api/clients/reset", s.handleClientsReset)
	mux.HandleFunc("GET "+p+"/api/upstream", s.handleUpstreamGet)
	mux.HandleFunc("PUT "+p+"/api/upstream", s.handleUpstreamSet)
	mux.HandleFunc("POST "+p+"/api/upstream", s.handleUpstreamSet)
	mux.HandleFunc("GET "+p+"/healthz", s.handleHealth)
	mux.HandleFunc("GET "+p+"/readyz", s.handleReady)
	// Unknown control-plane paths must answer here rather than reach upstream.
	mux.HandleFunc(p+"/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such control-plane endpoint: "+r.URL.Path)
	})
}

// PolicyView is the policy as the dashboard sees it.
//
// RPS is the number of requests permitted inside one window. With the default
// one-second window that is literally requests per second; with a longer window
// the sustained rate is RPS/WindowSeconds, reported as EffectiveRPS.
type PolicyView struct {
	RPS           int     `json:"rps"`
	WindowSeconds float64 `json:"window_seconds"`
	Limit         int     `json:"limit"`
	EffectiveRPS  float64 `json:"effective_rps"`
}

func policyView(p limiter.Policy) PolicyView {
	secs := p.Window.Seconds()
	eff := float64(p.Limit)
	if secs > 0 {
		eff = float64(p.Limit) / secs
	}
	return PolicyView{RPS: p.Limit, WindowSeconds: secs, Limit: p.Limit, EffectiveRPS: eff}
}

// StatsPayload is the document returned by /api/stats and pushed over SSE.
type StatsPayload struct {
	Timestamp   int64            `json:"ts"`
	Version     string           `json:"version"`
	Policy      PolicyView       `json:"policy"`
	Traffic     metrics.Snapshot `json:"traffic"`
	Limiter     limiter.Stats    `json:"limiter"`
	Upstream    string           `json:"upstream"`
	Subscribers int              `json:"subscribers"`
}

// Snapshot builds the current stats document.
func (s *Server) Snapshot() StatsPayload {
	upstream := "unconfigured"
	if s.proxy != nil && s.proxy.Upstream() != nil {
		upstream = s.proxy.Upstream().Target
	} else if s.upstream != nil {
		upstream = s.upstream.Target
	}
	return StatsPayload{
		Timestamp:   time.Now().UnixMilli(),
		Version:     s.version,
		Policy:      policyView(s.limiter.Policy()),
		Traffic:     s.metrics.Snapshot(50),
		Limiter:     s.limiter.Stats(),
		Upstream:    upstream,
		Subscribers: s.hub.Len(),
	}
}

func (s *Server) handleStats(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.Snapshot())
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	st := s.limiter.Stats()
	// The proxy is healthy even with Redis down: that is what the fallback is
	// for. Degradation is reported, not treated as failure.
	writeJSON(w, http.StatusOK, map[string]any{
		"status":         "ok",
		"version":        s.version,
		"uptime_seconds": time.Since(s.started).Seconds(),
		"backend":        st.Backend,
		"redis_enabled":  st.RedisEnabled,
		"redis_healthy":  st.RedisHealthy,
		"degraded":       st.Degraded,
	})
}

func (s *Server) handleReady(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready"})
}

func (s *Server) handlePolicyGet(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, policyView(s.limiter.Policy()))
}

// policyRequest is the accepted update body. Pointers distinguish "absent"
// from "zero" so a partial update leaves the other field untouched.
type policyRequest struct {
	RPS           *int     `json:"rps"`
	WindowSeconds *float64 `json:"window_seconds"`
}

func (s *Server) handlePolicySet(w http.ResponseWriter, r *http.Request) {
	req, err := decodePolicyRequest(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	next := s.limiter.Policy()
	if req.RPS != nil {
		next.Limit = *req.RPS
	}
	if req.WindowSeconds != nil {
		next.Window = time.Duration(*req.WindowSeconds * float64(time.Second))
	}
	if err := validatePolicy(next); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_policy", err.Error())
		return
	}

	applied := s.limiter.SetPolicy(next)
	s.log.Info("rate limit policy updated",
		"limit", applied.Limit, "window", applied.Window, "source", r.RemoteAddr)

	// Best effort: share the change with the rest of the fleet. A Redis outage
	// must not fail the update, since the local instance already applied it.
	shared := false
	shareErr := ""
	if s.store != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		if err := s.store.Save(ctx, applied); err != nil {
			shareErr = err.Error()
			s.log.Warn("could not share policy with other instances", "error", err)
		} else {
			shared = true
		}
		cancel()
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"policy":            policyView(applied),
		"shared_with_fleet": shared,
		"share_error":       shareErr,
	})
}

// decodePolicyRequest accepts either a JSON body or a form post, so the
// endpoint works from curl, fetch, and a plain HTML form alike.
func decodePolicyRequest(w http.ResponseWriter, r *http.Request) (policyRequest, error) {
	var req policyRequest
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "application/x-www-form-urlencoded") || strings.HasPrefix(ct, "multipart/form-data") {
		if err := r.ParseForm(); err != nil {
			return req, fmt.Errorf("could not parse form: %w", err)
		}
		if v := r.PostForm.Get("rps"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				return req, fmt.Errorf("rps must be an integer: %w", err)
			}
			req.RPS = &n
		}
		if v := r.PostForm.Get("window_seconds"); v != "" {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return req, fmt.Errorf("window_seconds must be a number: %w", err)
			}
			req.WindowSeconds = &f
		}
		if req.RPS == nil && req.WindowSeconds == nil {
			return req, errors.New("provide at least one of rps or window_seconds")
		}
		return req, nil
	}

	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return req, fmt.Errorf("could not parse JSON body: %w", err)
	}
	if req.RPS == nil && req.WindowSeconds == nil {
		return req, errors.New("provide at least one of rps or window_seconds")
	}
	return req, nil
}

func validatePolicy(p limiter.Policy) error {
	if p.Limit < 1 || p.Limit > 1_000_000 {
		return fmt.Errorf("rps must be in 1..1000000, got %d", p.Limit)
	}
	if p.Window < time.Second || p.Window > time.Hour {
		return fmt.Errorf("window_seconds must be in 1..3600, got %v", p.Window.Seconds())
	}
	return nil
}

// ClientsResponse is the payload of /api/clients.
type ClientsResponse struct {
	Policy PolicyView           `json:"policy"`
	Total  int                  `json:"total"`
	Rows   []metrics.ClientView `json:"rows"`
	// Fleet holds usage seen across every instance, read straight from Redis.
	// It is nil when Redis is unavailable or disabled.
	Fleet     map[string]int `json:"fleet,omitempty"`
	FleetNote string         `json:"fleet_note,omitempty"`
}

func (s *Server) handleClients(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	rows, total := s.metrics.Clients(time.Now(), limit)
	resp := ClientsResponse{
		Policy: policyView(s.limiter.Policy()),
		Total:  total,
		Rows:   rows,
	}

	if rl := s.limiter.Redis(); rl != nil && !s.limiter.Degraded() {
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		fleet, err := rl.ActiveKeys(ctx, 200)
		cancel()
		if err != nil {
			resp.FleetNote = "fleet view unavailable: " + err.Error()
		} else {
			resp.Fleet = fleet
		}
	} else {
		resp.FleetNote = "fleet view unavailable: serving from the in-memory fallback"
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleClientsReset(w http.ResponseWriter, r *http.Request) {
	s.metrics.ResetClients()
	s.limiter.Memory().Reset()

	deleted := 0
	note := ""
	if rl := s.limiter.Redis(); rl != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		n, err := rl.Reset(ctx)
		cancel()
		deleted = n
		if err != nil {
			note = err.Error()
			s.log.Warn("could not clear redis rate limit keys", "error", err)
		}
	}
	s.log.Info("client tracking reset", "redis_keys_deleted", deleted, "source", r.RemoteAddr)

	writeJSON(w, http.StatusOK, map[string]any{
		"status":             "reset",
		"redis_keys_deleted": deleted,
		"note":               note,
	})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	proxy.WriteJSON(w, status, payload)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	proxy.WriteJSON(w, status, map[string]any{"error": code, "message": message})
}

// UpstreamView is the upstream target as the dashboard sees it.
type UpstreamView struct {
	Target   string `json:"target"`
	RawURL   string `json:"raw_url"`
	Mock     bool   `json:"mock"`
	Editable bool   `json:"editable"`
}

func (s *Server) currentUpstream() *proxy.Upstream {
	if s.proxy != nil && s.proxy.Upstream() != nil {
		return s.proxy.Upstream()
	}
	return s.upstream
}

func (s *Server) upstreamView() UpstreamView {
	u := s.currentUpstream()
	v := UpstreamView{Editable: s.upstreamEditable && s.proxy != nil}
	if u != nil {
		v.Target, v.RawURL, v.Mock = u.Target, u.RawURL, u.Mock
	}
	return v
}

func (s *Server) handleUpstreamGet(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.upstreamView())
}

// upstreamRequest is the accepted body for changing the target.
type upstreamRequest struct {
	URL string `json:"url"`
}

func (s *Server) handleUpstreamSet(w http.ResponseWriter, r *http.Request) {
	if s.proxy == nil || !s.upstreamEditable {
		writeError(w, http.StatusForbidden, "upstream_locked",
			"changing the upstream at runtime is disabled; set UPSTREAM_EDITABLE=true to allow it")
		return
	}

	rawURL, err := decodeUpstreamRequest(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	// Build it exactly the way startup does, so validation and behaviour match.
	up, err := proxy.NewUpstream(rawURL, s.upstreamTimeout, s.log)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_upstream", err.Error())
		return
	}

	s.proxy.SetUpstream(up)
	s.log.Info("upstream target changed at runtime",
		"target", up.Target, "source", r.RemoteAddr)

	shared := false
	shareErr := ""
	if s.upstreamStore != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		if err := s.upstreamStore.Save(ctx, up.RawURL); err != nil {
			shareErr = err.Error()
			s.log.Warn("could not share upstream change with other instances", "error", err)
		} else {
			shared = true
		}
		cancel()
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"upstream":          s.upstreamView(),
		"shared_with_fleet": shared,
		"share_error":       shareErr,
	})
}

func decodeUpstreamRequest(w http.ResponseWriter, r *http.Request) (string, error) {
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "application/x-www-form-urlencoded") || strings.HasPrefix(ct, "multipart/form-data") {
		if err := r.ParseForm(); err != nil {
			return "", fmt.Errorf("could not parse form: %w", err)
		}
		v := strings.TrimSpace(r.PostForm.Get("url"))
		if v == "" {
			return "", errors.New("url is required")
		}
		return v, nil
	}
	var req upstreamRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return "", fmt.Errorf("could not parse JSON body: %w", err)
	}
	req.URL = strings.TrimSpace(req.URL)
	if req.URL == "" {
		return "", errors.New("url is required")
	}
	return req.URL, nil
}
