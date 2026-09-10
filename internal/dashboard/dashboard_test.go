package dashboard

import (
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func serve(t *testing.T) *httptest.Server { return serveAt(t, "") }

func serveAt(t *testing.T, prefix string) *httptest.Server {
	t.Helper()
	h, err := Handler()
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	mux := http.NewServeMux()
	Register(mux, prefix, h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func fetch(t *testing.T, srv *httptest.Server, path string) (*http.Response, string) {
	t.Helper()
	res, err := srv.Client().Get(srv.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("reading the body: %v", err)
	}
	return res, string(body)
}

func TestDashboardIsServedAtBothPaths(t *testing.T) {
	srv := serve(t)
	for _, path := range []string{Path, Path + "/"} {
		res, body := fetch(t, srv, path)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", path, res.StatusCode)
		}
		if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("%s: Content-Type = %q, want text/html", path, ct)
		}
		if !strings.Contains(body, "<!doctype html>") {
			t.Errorf("%s: the response does not look like the dashboard page", path)
		}
	}
}

func TestDashboardWiresUpTheControlPlane(t *testing.T) {
	srv := serve(t)
	_, body := fetch(t, srv, Path)

	// The page is the only client of these endpoints, so a rename that breaks
	// the UI should break this test too.
	for _, needle := range []string{
		`new EventSource(BASE + "/api/stream")`,
		`fetch(BASE + "/api/stats")`,
		`fetch(BASE + "/api/policy"`,
		`fetch(BASE + "/api/clients/reset"`,
		`id="stressPath"`,
		`id="chart"`,
		`id="policyForm"`,
		`id="clientRows"`,
		`id="stressForm"`,
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("the dashboard page is missing %q", needle)
		}
	}
}

// TestDashboardHasNoExternalDependencies keeps the page usable in an air-gapped
// deployment and consistent with the strict Content-Security-Policy below.
func TestDashboardHasNoExternalDependencies(t *testing.T) {
	srv := serve(t)
	_, body := fetch(t, srv, Path)

	external := regexp.MustCompile(`(?i)(src|href)\s*=\s*["']https?://`)
	if m := external.FindString(body); m != "" {
		t.Errorf("the dashboard references an external resource: %q", m)
	}
}

func TestDashboardSetsSecurityHeaders(t *testing.T) {
	srv := serve(t)
	res, _ := fetch(t, srv, Path)

	if got := res.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	csp := res.Header.Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("no Content-Security-Policy header on the dashboard")
	}
	for _, directive := range []string{"default-src 'none'", "connect-src 'self'"} {
		if !strings.Contains(csp, directive) {
			t.Errorf("Content-Security-Policy %q is missing %q", csp, directive)
		}
	}
}

// TestDashboardMountsOnlyUnderThePrefix mirrors the API regression test: a real
// upstream that owns /dashboard must keep it.
func TestDashboardMountsOnlyUnderThePrefix(t *testing.T) {
	srv := serveAt(t, "/_rl")

	for _, path := range []string{"/_rl/dashboard", "/_rl/dashboard/"} {
		res, body := fetch(t, srv, path)
		if res.StatusCode != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", path, res.StatusCode)
		}
		if !strings.Contains(body, "Rate Limiter Control Plane") {
			t.Errorf("%s: did not serve the dashboard page", path)
		}
	}

	if res, _ := fetch(t, srv, "/dashboard"); res.StatusCode != http.StatusNotFound {
		t.Errorf("/dashboard: status = %d, want 404: the upstream owns this path", res.StatusCode)
	}
}

// TestDashboardDerivesItsOwnBase pins the JavaScript that lets one static HTML
// file work at any prefix without Go templating.
func TestDashboardDerivesItsOwnBase(t *testing.T) {
	srv := serve(t)
	_, body := fetch(t, srv, Path)
	for _, needle := range []string{
		`var path = location.pathname;`,
		`var at = path.indexOf("/dashboard");`,
		`return at >= 0 ? path.slice(0, at) : "";`,
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("the base-path derivation changed, missing %q", needle)
		}
	}
	// lastIndexOf would compute "/_rl/dashboard" for "/_rl/dashboard/dashboard-x".
	if strings.Contains(body, `lastIndexOf("/dashboard")`) {
		t.Error("base derivation uses lastIndexOf, which breaks on a nested /dashboard path")
	}
}

// TestDashboardHasUpstreamControls pins the runtime upstream UI so a rename of
// its endpoint or element ids breaks this test rather than the page.
func TestDashboardHasUpstreamControls(t *testing.T) {
	srv := serve(t)
	_, body := fetch(t, srv, Path)
	for _, needle := range []string{
		`id="upstreamForm"`,
		`id="upstreamUrl"`,
		`id="openSite"`,
		`fetch(BASE + "/api/upstream"`,
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("the dashboard is missing the upstream control %q", needle)
		}
	}
}
