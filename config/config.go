// Package config loads and validates the proxy configuration from environment
// variables. Every value has a sane production default so the binary can be
// started with no configuration at all.
package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the fully resolved runtime configuration.
type Config struct {
	// Server
	Port            int
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
	ShutdownTimeout time.Duration

	// Redis
	RedisAddr     string
	RedisPassword string
	RedisDB       int
	RedisTimeout  time.Duration
	RedisPrefix   string

	// Rate limiting policy
	RateLimitRPS      int
	WindowSizeSeconds int

	// Fallback / circuit breaker
	FailureThreshold int
	FallbackCooldown time.Duration
	ProbeInterval    time.Duration
	CleanupInterval  time.Duration

	// Proxy
	UpstreamURL       string
	UpstreamTimeout   time.Duration
	TrustProxyHeaders bool
	// UpstreamEditable allows the target origin to be changed at runtime from
	// the dashboard. It is convenient for a demo but is an SSRF / open-relay
	// lever, so it can be turned off in a hardened deployment.
	UpstreamEditable bool

	// Dashboard and control plane
	DashboardEnabled bool
	MetricsWindow    int // seconds of history retained for the live graph
	// AdminPrefix namespaces every control-plane route so the proxy cannot
	// swallow paths belonging to the upstream application. Empty means mount
	// at the root, which is only safe when the upstream owns no /api or
	// /dashboard of its own.
	AdminPrefix string
	// AdminAddr, when set, binds the control plane and dashboard to their own
	// listener (e.g. "127.0.0.1:9090") instead of sharing the public port. The
	// public port then serves only proxied traffic, so the unauthenticated
	// dashboard is never exposed alongside it. This is the recommended shape
	// for a deployment that sits in front of a real application.
	AdminAddr string

	LogLevel string
}

// DefaultAdminPrefix namespaces the control plane. It is deliberately ugly: a
// real application is very unlikely to own a path starting with "/_rl", whereas
// "/api" and "/dashboard" are among the first paths any web app claims.
const DefaultAdminPrefix = "/_rl"

// NormalizePrefix canonicalizes an admin prefix to "" or "/segment[/segment]".
func NormalizePrefix(v string) string {
	v = strings.TrimSpace(v)
	v = strings.Trim(v, "/")
	if v == "" {
		return ""
	}
	return "/" + v
}

// AdminSeparate reports whether the control plane binds to its own listener.
func (c Config) AdminSeparate() bool { return strings.TrimSpace(c.AdminAddr) != "" }

// AdminURL builds an absolute local URL for a control-plane path such as
// "/dashboard" or "/api/stats".
func (c Config) AdminURL(path string) string {
	if c.AdminSeparate() {
		host := c.AdminAddr
		if strings.HasPrefix(host, ":") {
			host = "localhost" + host
		}
		return "http://" + host + path
	}
	return fmt.Sprintf("http://localhost:%d%s%s", c.Port, c.AdminPrefix, path)
}

// Window returns the configured sliding window as a duration.
func (c Config) Window() time.Duration {
	return time.Duration(c.WindowSizeSeconds) * time.Second
}

// Addr returns the listen address for the HTTP server.
func (c Config) Addr() string { return fmt.Sprintf(":%d", c.Port) }

// RedisEnabled reports whether a Redis backend was configured. When it is
// disabled the proxy runs purely on the in-memory limiter.
func (c Config) RedisEnabled() bool {
	return c.RedisAddr != "" && !strings.EqualFold(c.RedisAddr, "disabled")
}

// Load reads configuration from the process environment.
func Load() (Config, error) {
	c := Config{
		Port:            envInt("PORT", 8080),
		ReadTimeout:     envDuration("READ_TIMEOUT", 15*time.Second),
		WriteTimeout:    envDuration("WRITE_TIMEOUT", 0), // 0: SSE streams must not be cut off
		IdleTimeout:     envDuration("IDLE_TIMEOUT", 60*time.Second),
		ShutdownTimeout: envDuration("SHUTDOWN_TIMEOUT", 10*time.Second),

		RedisAddr:     envString("REDIS_ADDR", "localhost:6379"),
		RedisPassword: envString("REDIS_PASSWORD", ""),
		RedisDB:       envInt("REDIS_DB", 0),
		RedisTimeout:  envDuration("REDIS_TIMEOUT", 200*time.Millisecond),
		RedisPrefix:   envString("REDIS_PREFIX", "rl:"),

		RateLimitRPS:      envInt("RATE_LIMIT_RPS", 10),
		WindowSizeSeconds: envInt("WINDOW_SIZE_SECONDS", 1),

		FailureThreshold: envInt("FAILURE_THRESHOLD", 3),
		FallbackCooldown: envDuration("FALLBACK_COOLDOWN", 5*time.Second),
		ProbeInterval:    envDuration("PROBE_INTERVAL", 2*time.Second),
		CleanupInterval:  envDuration("CLEANUP_INTERVAL", 30*time.Second),

		UpstreamURL:       envString("UPSTREAM_URL", "mock://internal"),
		UpstreamTimeout:   envDuration("UPSTREAM_TIMEOUT", 30*time.Second),
		TrustProxyHeaders: envBool("TRUST_PROXY_HEADERS", false),
		UpstreamEditable:  envBool("UPSTREAM_EDITABLE", true),

		DashboardEnabled: envBool("DASHBOARD_ENABLED", true),
		MetricsWindow:    envInt("METRICS_WINDOW_SECONDS", 60),
		AdminPrefix:      NormalizePrefix(envString("ADMIN_PREFIX", DefaultAdminPrefix)),
		AdminAddr:        envString("ADMIN_ADDR", ""),

		LogLevel: envString("LOG_LEVEL", "info"),
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// Validate rejects configurations that cannot produce a working service.
func (c Config) Validate() error {
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("PORT must be in 1..65535, got %d", c.Port)
	}
	if c.RateLimitRPS < 1 {
		return fmt.Errorf("RATE_LIMIT_RPS must be >= 1, got %d", c.RateLimitRPS)
	}
	if c.WindowSizeSeconds < 1 {
		return fmt.Errorf("WINDOW_SIZE_SECONDS must be >= 1, got %d", c.WindowSizeSeconds)
	}
	if c.RedisTimeout <= 0 {
		return fmt.Errorf("REDIS_TIMEOUT must be > 0, got %s", c.RedisTimeout)
	}
	if c.FailureThreshold < 1 {
		return fmt.Errorf("FAILURE_THRESHOLD must be >= 1, got %d", c.FailureThreshold)
	}
	if c.MetricsWindow < 10 || c.MetricsWindow > 3600 {
		return fmt.Errorf("METRICS_WINDOW_SECONDS must be in 10..3600, got %d", c.MetricsWindow)
	}
	if c.UpstreamURL == "" {
		return fmt.Errorf("UPSTREAM_URL must not be empty")
	}
	if err := validateAdminPrefix(c.AdminPrefix); err != nil {
		return err
	}
	if c.AdminSeparate() {
		if _, _, err := net.SplitHostPort(strings.TrimSpace(c.AdminAddr)); err != nil {
			return fmt.Errorf("ADMIN_ADDR must be host:port (e.g. 127.0.0.1:9090), got %q", c.AdminAddr)
		}
		if strings.TrimSpace(c.AdminAddr) == c.Addr() {
			return fmt.Errorf("ADMIN_ADDR %q must differ from the public port %q", c.AdminAddr, c.Addr())
		}
	}
	return nil
}

// validateAdminPrefix rejects values that http.ServeMux would parse as
// something other than a literal path segment.
//
// The dangerous case is braces: Go 1.22 patterns treat {name} as a wildcard, so
// ADMIN_PREFIX="/{tenant}" would register "GET /{tenant}/api/stats" and swallow
// /customers/api/stats from the upstream - a broader hijack than the collision
// this prefix exists to prevent. A value with no leading slash is just as bad in
// a quieter way: ServeMux reads it as a HOST pattern, so the control plane
// answers only for that Host and is invisible everywhere else. NormalizePrefix
// fixes the second; this rejects the rest loudly.
func validateAdminPrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	if !strings.HasPrefix(prefix, "/") {
		return fmt.Errorf("ADMIN_PREFIX must start with a slash, got %q", prefix)
	}
	for _, bad := range []string{"{", "}", "?", "#", "..", " ", "	", "//"} {
		if strings.Contains(prefix, bad) {
			return fmt.Errorf("ADMIN_PREFIX must be a literal path, %q contains %q", prefix, bad)
		}
	}
	if strings.Contains(prefix, "/dashboard") {
		return fmt.Errorf("ADMIN_PREFIX must not contain %q, it would confuse the dashboard base-path derivation, got %q", "/dashboard", prefix)
	}
	return nil
}

// Redact returns a copy safe to log (secrets removed).
func (c Config) Redact() Config {
	if c.RedisPassword != "" {
		c.RedisPassword = "***"
	}
	return c
}

func envString(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func envInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok {
		if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			return b
		}
	}
	return def
}

// envDuration accepts either a Go duration string ("200ms") or a bare number of
// seconds ("30").
func envDuration(key string, def time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	v = strings.TrimSpace(v)
	if v == "" {
		return def
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(n) * time.Second
	}
	return def
}
