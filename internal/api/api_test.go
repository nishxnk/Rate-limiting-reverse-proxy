package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/avd450/ratelimit-proxy/internal/limiter"
	"github.com/avd450/ratelimit-proxy/internal/metrics"
	"github.com/avd450/ratelimit-proxy/internal/proxy"
)

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

type fixture struct {
	server  *httptest.Server
	manager *limiter.Manager
	metrics *metrics.Collector
	api     *Server
}

func newFixture(t *testing.T) *fixture { return newFixtureAt(t, "") }

func newFixtureAt(t *testing.T, prefix string) *fixture {
	t.Helper()
	mgr := limiter.NewManager(nil, limiter.NewMemoryLimiter(0), limiter.ManagerConfig{
		Policy: limiter.Policy{Limit: 10, Window: time.Second},
		Logger: quiet(),
	})
	t.Cleanup(func() {
		if err := mgr.Close(); err != nil {
			t.Errorf("closing the limiter: %v", err)
		}
	})

	up, err := proxy.NewUpstream("mock://internal", time.Second, quiet())
	if err != nil {
		t.Fatalf("NewUpstream: %v", err)
	}
	coll := metrics.NewCollector(60, time.Minute)
	hub := metrics.NewHub(4)

	srv, err := New(Options{
		Limiter: mgr, Metrics: coll, Hub: hub, Upstream: up, Logger: quiet(), Version: "test",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	mux := http.NewServeMux()
	srv.Register(mux, prefix)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	return &fixture{server: ts, manager: mgr, metrics: coll, api: srv}
}

func (f *fixture) do(t *testing.T, method, path, contentType string, body io.Reader) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, f.server.URL+path, body)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	res, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("reading the body: %v", err)
	}
	return res, raw
}

func TestStatsReflectsRecordedTraffic(t *testing.T) {
	f := newFixture(t)
	f.metrics.Record(metrics.Event{
		Key: "ip:203.0.113.9", Kind: "ip", Allowed: true, Status: 200,
		Limit: 10, Remaining: 9, ResetAt: time.Now().Add(time.Second),
		Backend: limiter.BackendMemory, Latency: 3 * time.Millisecond,
	})
	f.metrics.Record(metrics.Event{
		Key: "ip:203.0.113.9", Kind: "ip", Allowed: false, Status: 429,
		Limit: 10, Backend: limiter.BackendMemory, Latency: time.Millisecond,
	})

	res, body := f.do(t, http.MethodGet, "/api/stats", "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", res.StatusCode, body)
	}

	var payload StatsPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decoding stats: %v (body %s)", err, body)
	}
	if payload.Traffic.Total != 2 || payload.Traffic.Allowed != 1 || payload.Traffic.Blocked != 1 {
		t.Errorf("traffic = %+v, want 2 total, 1 allowed, 1 blocked", payload.Traffic)
	}
	if payload.Policy.RPS != 10 || payload.Policy.WindowSeconds != 1 {
		t.Errorf("policy = %+v, want 10 requests per 1s", payload.Policy)
	}
	if len(payload.Traffic.Series) != 60 {
		t.Errorf("series length = %d, want 60 seconds of history", len(payload.Traffic.Series))
	}
	if len(payload.Traffic.Clients) != 1 || payload.Traffic.Clients[0].Blocked != 1 {
		t.Errorf("clients = %+v, want one row with 1 blocked request", payload.Traffic.Clients)
	}
	if payload.Version != "test" || payload.Upstream != "mock://internal" {
		t.Errorf("payload = version %q upstream %q, want test and mock://internal", payload.Version, payload.Upstream)
	}
}

func TestPolicyUpdateAppliesToTheLimiter(t *testing.T) {
	f := newFixture(t)

	res, body := f.do(t, http.MethodPut, "/api/policy", "application/json",
		strings.NewReader(`{"rps":25,"window_seconds":5}`))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", res.StatusCode, body)
	}

	var out struct {
		Policy PolicyView `json:"policy"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decoding the response: %v (body %s)", err, body)
	}
	if out.Policy.RPS != 25 || out.Policy.WindowSeconds != 5 {
		t.Errorf("returned policy = %+v, want 25 per 5s", out.Policy)
	}
	if out.Policy.EffectiveRPS != 5 {
		t.Errorf("effective rps = %v, want 5", out.Policy.EffectiveRPS)
	}

	got := f.manager.Policy()
	if got.Limit != 25 || got.Window != 5*time.Second {
		t.Errorf("limiter policy = %+v, want limit 25 and a 5s window", got)
	}

	// A partial update must leave the untouched field alone.
	if res, body := f.do(t, http.MethodPut, "/api/policy", "application/json",
		strings.NewReader(`{"rps":7}`)); res.StatusCode != http.StatusOK {
		t.Fatalf("partial update: status = %d (body %s)", res.StatusCode, body)
	}
	if got := f.manager.Policy(); got.Limit != 7 || got.Window != 5*time.Second {
		t.Errorf("after a partial update the policy is %+v, want limit 7 and a 5s window", got)
	}
}

func TestPolicyUpdateAcceptsAFormPost(t *testing.T) {
	f := newFixture(t)
	form := url.Values{"rps": {"33"}, "window_seconds": {"2"}}

	res, body := f.do(t, http.MethodPost, "/api/policy",
		"application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", res.StatusCode, body)
	}
	if got := f.manager.Policy(); got.Limit != 33 || got.Window != 2*time.Second {
		t.Errorf("policy = %+v, want limit 33 and a 2s window", got)
	}
}

func TestPolicyUpdateRejectsBadInput(t *testing.T) {
	f := newFixture(t)
	before := f.manager.Policy()

	cases := map[string]string{
		"zero rps":         `{"rps":0}`,
		"negative rps":     `{"rps":-5}`,
		"rps far too big":  `{"rps":9999999}`,
		"window too long":  `{"window_seconds":7200}`,
		"window too small": `{"window_seconds":0}`,
		"empty body":       `{}`,
		"unknown field":    `{"burst":10}`,
		"not json":         `nonsense`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			res, raw := f.do(t, http.MethodPut, "/api/policy", "application/json", strings.NewReader(body))
			if res.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", res.StatusCode, raw)
			}
			var payload struct {
				Error   string `json:"error"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal(raw, &payload); err != nil || payload.Message == "" {
				t.Errorf("body = %s, want a JSON error with a message (err %v)", raw, err)
			}
		})
	}

	if got := f.manager.Policy(); !got.Equal(before) {
		t.Errorf("a rejected update changed the policy to %+v, want it left at %+v", got, before)
	}
}

func TestClientsEndpointListsTrackedKeys(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 3; i++ {
		f.metrics.Record(metrics.Event{
			Key: "api_key:tenant-a", Kind: "api_key", Allowed: true, Status: 200,
			Limit: 10, Remaining: 9 - i, ResetAt: time.Now().Add(time.Second),
			Backend: limiter.BackendMemory, Latency: time.Millisecond,
		})
	}
	f.metrics.Record(metrics.Event{
		Key: "ip:203.0.113.9", Kind: "ip", Allowed: false, Status: 429,
		Limit: 10, Backend: limiter.BackendMemory,
	})

	res, body := f.do(t, http.MethodGet, "/api/clients", "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", res.StatusCode, body)
	}
	var payload ClientsResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decoding clients: %v (body %s)", err, body)
	}
	if payload.Total != 2 || len(payload.Rows) != 2 {
		t.Fatalf("total = %d with %d rows, want 2 and 2", payload.Total, len(payload.Rows))
	}
	// Rows are ordered by traffic, so the busiest client comes first.
	if payload.Rows[0].Key != "api_key:tenant-a" || payload.Rows[0].Requests != 3 {
		t.Errorf("first row = %+v, want tenant-a with 3 requests", payload.Rows[0])
	}
	if payload.FleetNote == "" {
		t.Error("fleet_note is empty, want an explanation that Redis is not backing this view")
	}
}

func TestClientsResetClearsTracking(t *testing.T) {
	f := newFixture(t)
	f.metrics.Record(metrics.Event{
		Key: "ip:203.0.113.9", Kind: "ip", Allowed: true, Status: 200, Limit: 10,
	})
	if _, total := f.metrics.Clients(time.Now(), 10); total != 1 {
		t.Fatalf("tracked clients = %d, want 1 before the reset", total)
	}

	res, body := f.do(t, http.MethodPost, "/api/clients/reset", "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", res.StatusCode, body)
	}
	if _, total := f.metrics.Clients(time.Now(), 10); total != 0 {
		t.Errorf("tracked clients = %d, want 0 after the reset", total)
	}
}

func TestHealthReportsTheActiveBackend(t *testing.T) {
	f := newFixture(t)
	for _, path := range []string{"/api/health", "/healthz"} {
		res, body := f.do(t, http.MethodGet, path, "", nil)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200 (body %s)", path, res.StatusCode, body)
		}
		var payload struct {
			Status   string `json:"status"`
			Backend  string `json:"backend"`
			Degraded bool   `json:"degraded"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("%s: decoding: %v (body %s)", path, err, body)
		}
		if payload.Status != "ok" || payload.Backend != limiter.BackendMemory {
			t.Errorf("%s: payload = %+v, want ok on the memory backend", path, payload)
		}
	}
}

// TestStreamPushesSnapshots covers the SSE transport end to end: the handler
// must send the current state immediately and keep pushing on each broadcast.
func TestStreamPushesSnapshots(t *testing.T) {
	f := newFixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// The broadcaster drives the periodic frames.
	go f.api.StartBroadcaster(ctx, 50*time.Millisecond)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.server.URL+"/api/stream", nil)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	res, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatalf("opening the stream: %v", err)
	}
	defer res.Body.Close()

	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	frames := make(chan StatsPayload, 4)
	errs := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(res.Body)
		var data []byte
		for {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				errs <- err
				return
			}
			switch {
			case bytes.HasPrefix(line, []byte("data: ")):
				data = bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data: ")))
			case len(bytes.TrimSpace(line)) == 0 && data != nil:
				var payload StatsPayload
				if err := json.Unmarshal(data, &payload); err != nil {
					errs <- err
					return
				}
				data = nil
				select {
				case frames <- payload:
				default:
					return
				}
			}
		}
	}()

	// The first frame arrives without waiting for a tick.
	select {
	case p := <-frames:
		if p.Version != "test" {
			t.Errorf("first frame version = %q, want test", p.Version)
		}
		if len(p.Traffic.Series) == 0 {
			t.Error("first frame carries no traffic series")
		}
	case err := <-errs:
		t.Fatalf("reading the stream: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the initial snapshot")
	}

	// Traffic recorded after the connection opened must show up in a later frame.
	f.metrics.Record(metrics.Event{
		Key: "ip:203.0.113.9", Kind: "ip", Allowed: false, Status: 429, Limit: 10,
	})
	deadline := time.After(5 * time.Second)
	for {
		select {
		case p := <-frames:
			if p.Traffic.Blocked == 1 {
				return
			}
		case err := <-errs:
			t.Fatalf("reading the stream: %v", err)
		case <-deadline:
			t.Fatal("timed out waiting for a frame carrying the new traffic")
		}
	}
}

func TestUnknownControlPlanePathIsNotProxied(t *testing.T) {
	f := newFixture(t)
	// The mux in main sends unmatched /api/ paths to a JSON 404; here the bare
	// fixture mux has no catch-all, so the standard 404 is expected instead.
	res, _ := f.do(t, http.MethodGet, "/api/nope", "", nil)
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", res.StatusCode)
	}
}

// TestControlPlaneLivesOnlyUnderThePrefix is the regression test for the bug
// that broke a real upstream: the control plane used to squat on /api and
// /dashboard at the root, swallowing the proxied application requests.
func TestControlPlaneLivesOnlyUnderThePrefix(t *testing.T) {
	f := newFixtureAt(t, "/_rl")

	// Served where it belongs.
	for _, path := range []string{"/_rl/api/stats", "/_rl/api/policy", "/_rl/api/clients", "/_rl/healthz", "/_rl/readyz"} {
		if res, body := f.do(t, http.MethodGet, path, "", nil); res.StatusCode != http.StatusOK {
			t.Errorf("%s: status = %d, want 200 (body %s)", path, res.StatusCode, body)
		}
	}
	// Unknown paths under the prefix are still ours, and answer JSON.
	res, body := f.do(t, http.MethodGet, "/_rl/api/nope", "", nil)
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("/_rl/api/nope: status = %d, want 404", res.StatusCode)
	}
	if !strings.Contains(string(body), "control-plane") {
		t.Errorf("/_rl/api/nope body = %s, want the control-plane 404", body)
	}

	// Nothing is claimed at the root, so these fall through to the proxy in the
	// real server. Here the bare fixture mux has no catch-all, so a stdlib 404
	// with a non-JSON body proves the route was never registered.
	for _, path := range []string{"/api/stats", "/api/health", "/api/users", "/healthz", "/readyz"} {
		res, body := f.do(t, http.MethodGet, path, "", nil)
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404: the control plane must not claim root paths", path, res.StatusCode)
		}
		if strings.Contains(string(body), "control-plane") {
			t.Errorf("%s: answered by the control plane (%s), want it left for the upstream", path, body)
		}
	}
}

// newFixtureWithProxy wires a real proxy handler so the upstream endpoints work.
func newFixtureWithProxy(t *testing.T) *fixture {
	t.Helper()
	f := newFixtureAt(t, "")
	up, err := proxy.NewUpstream("mock://internal", time.Second, quiet())
	if err != nil {
		t.Fatalf("NewUpstream: %v", err)
	}
	ph, err := proxy.New(proxy.Options{Limiter: f.manager, Metrics: f.metrics, Upstream: up, Logger: quiet()})
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	srv, err := New(Options{
		Limiter: f.manager, Metrics: f.metrics, Hub: metrics.NewHub(4),
		Upstream: up, Proxy: ph, UpstreamTimeout: time.Second, UpstreamEditable: true,
		Logger: quiet(), Version: "test",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	srv.Register(mux, "")
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	f.server = ts
	f.api = srv
	return f
}

func TestUpstreamCanBeChangedAtRuntime(t *testing.T) {
	f := newFixtureWithProxy(t)

	res, body := f.do(t, http.MethodGet, "/api/upstream", "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET upstream: status %d (body %s)", res.StatusCode, body)
	}
	var view UpstreamView
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !view.Mock || !view.Editable {
		t.Errorf("initial upstream = %+v, want the editable mock", view)
	}

	up := f.api.currentUpstream()
	res, body = f.do(t, http.MethodPut, "/api/upstream", "application/json",
		strings.NewReader(`{"url":"https://example.com/api"}`))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("PUT upstream: status %d (body %s)", res.StatusCode, body)
	}
	if got := f.api.currentUpstream().Target; got != "https://example.com/api" {
		t.Errorf("upstream target = %q, want https://example.com/api", got)
	}
	if f.api.currentUpstream() == up {
		t.Error("the upstream pointer was not swapped")
	}
}

func TestUpstreamChangeRejectsBadURLs(t *testing.T) {
	f := newFixtureWithProxy(t)
	before := f.api.currentUpstream().Target

	for _, body := range []string{`{"url":"ftp://nope"}`, `{"url":""}`, `{"url":"http://"}`, `{"bad":"field"}`, `not json`} {
		res, raw := f.do(t, http.MethodPut, "/api/upstream", "application/json", strings.NewReader(body))
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("PUT %s: status %d, want 400 (body %s)", body, res.StatusCode, raw)
		}
	}
	if got := f.api.currentUpstream().Target; got != before {
		t.Errorf("a rejected change moved the upstream to %q, want it left at %q", got, before)
	}
}

func TestUpstreamChangeForbiddenWhenLocked(t *testing.T) {
	// The default fixture has no proxy handle, which is how a locked-down
	// deployment (UPSTREAM_EDITABLE=false) presents to the endpoint.
	f := newFixture(t)
	res, _ := f.do(t, http.MethodPut, "/api/upstream", "application/json",
		strings.NewReader(`{"url":"https://example.com"}`))
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("PUT upstream with no proxy handle: status %d, want 403", res.StatusCode)
	}
}

// newFixtureWithToken wires the control plane with an admin token set.
func newFixtureWithToken(t *testing.T, token string) *fixture {
	t.Helper()
	f := newFixtureAt(t, "")
	srv, err := New(Options{
		Limiter: f.manager, Metrics: f.metrics, Hub: metrics.NewHub(4),
		Upstream: nil, AdminToken: token, Logger: quiet(), Version: "test",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	srv.Register(mux, "")
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	f.server = ts
	f.api = srv
	return f
}

func TestAdminTokenProtectsControlPlane(t *testing.T) {
	const token = "s3cret-token"
	f := newFixtureWithToken(t, token)

	// No credentials -> 401 with a Basic challenge so browsers prompt.
	res, _ := f.do(t, http.MethodGet, "/api/stats", "", nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: status %d, want 401", res.StatusCode)
	}
	if ch := res.Header.Get("WWW-Authenticate"); !strings.Contains(ch, "Basic") {
		t.Errorf("WWW-Authenticate = %q, want a Basic challenge", ch)
	}

	// Each accepted credential form should unlock it.
	forms := []struct {
		name string
		set  func(*http.Request)
	}{
		{"bearer", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }},
		{"x-admin-token", func(r *http.Request) { r.Header.Set("X-Admin-Token", token) }},
		{"basic", func(r *http.Request) { r.SetBasicAuth("admin", token) }},
	}
	for _, tc := range forms {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, f.server.URL+"/api/stats", nil)
			tc.set(req)
			res, err := f.server.Client().Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer res.Body.Close()
			if res.StatusCode != http.StatusOK {
				t.Errorf("%s: status %d, want 200", tc.name, res.StatusCode)
			}
		})
	}

	// A wrong token stays rejected.
	req, _ := http.NewRequest(http.MethodGet, f.server.URL+"/api/stats", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	res, _ = f.server.Client().Do(req)
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong token: status %d, want 401", res.StatusCode)
	}
}

func TestAdminTokenLeavesHealthOpen(t *testing.T) {
	f := newFixtureWithToken(t, "s3cret-token")
	// Container / LB probes must work without the token.
	for _, path := range []string{"/healthz", "/readyz", "/api/health"} {
		if res, _ := f.do(t, http.MethodGet, path, "", nil); res.StatusCode != http.StatusOK {
			t.Errorf("%s: status %d, want 200 without a token", path, res.StatusCode)
		}
	}
}

func TestNoTokenMeansOpen(t *testing.T) {
	// The default (empty token) keeps the control plane open, as before.
	f := newFixture(t)
	if res, _ := f.do(t, http.MethodGet, "/api/stats", "", nil); res.StatusCode != http.StatusOK {
		t.Errorf("status %d, want 200 when no token is configured", res.StatusCode)
	}
}
