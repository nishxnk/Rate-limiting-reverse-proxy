package config

import (
	"testing"
	"time"
)

func TestLoadUsesDefaultsWhenTheEnvironmentIsEmpty(t *testing.T) {
	for _, k := range []string{
		"PORT", "REDIS_ADDR", "RATE_LIMIT_RPS", "WINDOW_SIZE_SECONDS",
		"UPSTREAM_URL", "REDIS_TIMEOUT", "DASHBOARD_ENABLED", "TRUST_PROXY_HEADERS",
	} {
		t.Setenv(k, "")
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != 8080 {
		t.Errorf("Port = %d, want 8080", cfg.Port)
	}
	if cfg.RateLimitRPS != 10 || cfg.WindowSizeSeconds != 1 {
		t.Errorf("policy = %d per %ds, want 10 per 1s", cfg.RateLimitRPS, cfg.WindowSizeSeconds)
	}
	if cfg.RedisTimeout != 200*time.Millisecond {
		t.Errorf("RedisTimeout = %v, want 200ms", cfg.RedisTimeout)
	}
	if cfg.UpstreamURL != "mock://internal" {
		t.Errorf("UpstreamURL = %q, want the built-in mock", cfg.UpstreamURL)
	}
	if !cfg.DashboardEnabled {
		t.Error("DashboardEnabled = false, want the dashboard on by default")
	}
	if cfg.TrustProxyHeaders {
		t.Error("TrustProxyHeaders = true, want forwarding headers distrusted by default")
	}
	if cfg.Window() != time.Second {
		t.Errorf("Window() = %v, want 1s", cfg.Window())
	}
	if cfg.Addr() != ":8080" {
		t.Errorf("Addr() = %q, want :8080", cfg.Addr())
	}
}

func TestLoadReadsTheEnvironment(t *testing.T) {
	t.Setenv("PORT", "9999")
	t.Setenv("REDIS_ADDR", "redis.internal:6379")
	t.Setenv("REDIS_PASSWORD", "hunter2")
	t.Setenv("RATE_LIMIT_RPS", "250")
	t.Setenv("WINDOW_SIZE_SECONDS", "30")
	t.Setenv("UPSTREAM_URL", "https://httpbin.org/anything")
	t.Setenv("TRUST_PROXY_HEADERS", "true")
	t.Setenv("DASHBOARD_ENABLED", "false")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != 9999 || cfg.RedisAddr != "redis.internal:6379" {
		t.Errorf("cfg = port %d addr %q, want 9999 and redis.internal:6379", cfg.Port, cfg.RedisAddr)
	}
	if cfg.RateLimitRPS != 250 || cfg.Window() != 30*time.Second {
		t.Errorf("policy = %d per %v, want 250 per 30s", cfg.RateLimitRPS, cfg.Window())
	}
	if !cfg.TrustProxyHeaders || cfg.DashboardEnabled {
		t.Errorf("flags = trust %v dashboard %v, want true and false", cfg.TrustProxyHeaders, cfg.DashboardEnabled)
	}
	if got := cfg.Redact().RedisPassword; got != "***" {
		t.Errorf("Redact().RedisPassword = %q, want the secret masked", got)
	}
	if cfg.RedisPassword != "hunter2" {
		t.Error("Redact mutated the original configuration")
	}
}

func TestDurationsAcceptSecondsOrGoSyntax(t *testing.T) {
	t.Setenv("REDIS_TIMEOUT", "350ms")
	t.Setenv("SHUTDOWN_TIMEOUT", "45")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RedisTimeout != 350*time.Millisecond {
		t.Errorf("RedisTimeout = %v, want 350ms", cfg.RedisTimeout)
	}
	if cfg.ShutdownTimeout != 45*time.Second {
		t.Errorf("ShutdownTimeout = %v, want 45s from a bare seconds value", cfg.ShutdownTimeout)
	}
}

func TestRedisCanBeDisabled(t *testing.T) {
	for _, v := range []string{"disabled", "DISABLED"} {
		t.Setenv("REDIS_ADDR", v)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.RedisEnabled() {
			t.Errorf("REDIS_ADDR=%q: RedisEnabled() = true, want false", v)
		}
	}
}

func TestValidateRejectsImpossibleConfigurations(t *testing.T) {
	base := func() Config {
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		return cfg
	}

	cases := map[string]func(*Config){
		"port out of range":   func(c *Config) { c.Port = 70000 },
		"zero rate limit":     func(c *Config) { c.RateLimitRPS = 0 },
		"zero window":         func(c *Config) { c.WindowSizeSeconds = 0 },
		"zero redis timeout":  func(c *Config) { c.RedisTimeout = 0 },
		"no upstream":         func(c *Config) { c.UpstreamURL = "" },
		"tiny metrics window": func(c *Config) { c.MetricsWindow = 1 },
		"zero threshold":      func(c *Config) { c.FailureThreshold = 0 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := base()
			mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Errorf("Validate() = nil, want an error for %s", name)
			}
		})
	}
}

func TestUnparseableValuesFallBackToDefaults(t *testing.T) {
	t.Setenv("PORT", "not-a-number")
	t.Setenv("TRUST_PROXY_HEADERS", "maybe")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != 8080 {
		t.Errorf("Port = %d, want the default 8080 when the value cannot be parsed", cfg.Port)
	}
	if cfg.TrustProxyHeaders {
		t.Error("TrustProxyHeaders = true, want the default false for an unparseable value")
	}
}

func TestNormalizePrefix(t *testing.T) {
	cases := map[string]string{
		"":            "",
		"/":           "",
		"///":         "",
		"_rl":         "/_rl",
		"/_rl":        "/_rl",
		"/_rl/":       "/_rl",
		"  /_rl/  ":   "/_rl",
		"admin/inner": "/admin/inner",
	}
	for in, want := range cases {
		if got := NormalizePrefix(in); got != want {
			t.Errorf("NormalizePrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestAdminPrefixRejectsPatternSyntax guards the worst failure mode: Go 1.22
// ServeMux patterns treat {name} as a wildcard, so an unvalidated prefix could
// register "GET /{tenant}/api/stats" and swallow the upstream requests it exists
// to protect.
func TestAdminPrefixRejectsPatternSyntax(t *testing.T) {
	bad := []string{"/{tenant}", "/a}b", "/a b", "/a\tb", "/a?b", "/a#b", "/../etc", "/a//b", "/x/dashboard"}
	for _, prefix := range bad {
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		cfg.AdminPrefix = prefix
		if err := cfg.Validate(); err == nil {
			t.Errorf("Validate() accepted ADMIN_PREFIX %q, want it rejected", prefix)
		}
	}
	for _, prefix := range []string{"", "/_rl", "/internal/control"} {
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		cfg.AdminPrefix = prefix
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate() rejected ADMIN_PREFIX %q: %v", prefix, err)
		}
	}
}

func TestAdminPrefixDefaultsAndURL(t *testing.T) {
	t.Setenv("ADMIN_PREFIX", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AdminPrefix != DefaultAdminPrefix {
		t.Errorf("AdminPrefix = %q, want the default %q", cfg.AdminPrefix, DefaultAdminPrefix)
	}
	if got, want := cfg.AdminURL("/dashboard"), "http://localhost:8080/_rl/dashboard"; got != want {
		t.Errorf("AdminURL = %q, want %q", got, want)
	}

	t.Setenv("ADMIN_PREFIX", "/ops/rl/")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AdminPrefix != "/ops/rl" {
		t.Errorf("AdminPrefix = %q, want /ops/rl", cfg.AdminPrefix)
	}
}

func TestAdminAddrSeparateListener(t *testing.T) {
	t.Setenv("ADMIN_ADDR", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AdminSeparate() {
		t.Error("AdminSeparate() = true with no ADMIN_ADDR, want false")
	}
	// Shared listener: dashboard URL carries the prefix.
	if got, want := cfg.AdminURL("/dashboard"), "http://localhost:8080/_rl/dashboard"; got != want {
		t.Errorf("shared AdminURL = %q, want %q", got, want)
	}

	t.Setenv("ADMIN_ADDR", "127.0.0.1:9090")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.AdminSeparate() {
		t.Fatal("AdminSeparate() = false with ADMIN_ADDR set, want true")
	}
	// Separate listener: mounted at the root of its own address, no prefix.
	if got, want := cfg.AdminURL("/dashboard"), "http://127.0.0.1:9090/dashboard"; got != want {
		t.Errorf("separate AdminURL = %q, want %q", got, want)
	}
}

func TestAdminAddrValidation(t *testing.T) {
	base := func() Config {
		c, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		return c
	}
	// Not host:port.
	c := base()
	c.AdminAddr = "9090"
	if err := c.Validate(); err == nil {
		t.Error("Validate() accepted ADMIN_ADDR without a host, want an error")
	}
	// Same as the public port.
	c = base()
	c.Port = 8080
	c.AdminAddr = ":8080"
	if err := c.Validate(); err == nil {
		t.Error("Validate() accepted an ADMIN_ADDR equal to the public port, want an error")
	}
	// Valid.
	c = base()
	c.AdminAddr = "127.0.0.1:9090"
	if err := c.Validate(); err != nil {
		t.Errorf("Validate() rejected a valid ADMIN_ADDR: %v", err)
	}
}
