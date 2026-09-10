package api

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// authRealm is shown in the browser's HTTP Basic prompt.
const authRealm = "rate-limiter admin"

// authEnabled reports whether an admin token was configured.
func (s *Server) authEnabled() bool { return s.token != "" }

// authorized reports whether a request carries the admin token. A caller may
// present it three ways, so both browsers and scripts are covered:
//
//	Authorization: Bearer <token>
//	Authorization: Basic  <base64(anything:<token>)>   (browser prompt, curl -u)
//	X-Admin-Token: <token>
func (s *Server) authorized(r *http.Request) bool {
	if !s.authEnabled() {
		return true
	}
	if h := r.Header.Get("X-Admin-Token"); h != "" && tokenEqual(h, s.token) {
		return true
	}
	if h := r.Header.Get("Authorization"); h != "" {
		if rest, ok := cutPrefixFold(h, "Bearer "); ok && tokenEqual(strings.TrimSpace(rest), s.token) {
			return true
		}
	}
	if _, pass, ok := r.BasicAuth(); ok && tokenEqual(pass, s.token) {
		return true
	}
	return false
}

// guard wraps a control-plane handler so it only runs for authorized callers.
func (s *Server) guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			s.denyAuth(w)
			return
		}
		h(w, r)
	}
}

// GuardHandler is guard for an http.Handler, used to protect the dashboard page
// which is served from another package.
func (s *Server) GuardHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			s.denyAuth(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) denyAuth(w http.ResponseWriter) {
	// The Basic challenge makes a browser show a login box for the dashboard.
	w.Header().Set("WWW-Authenticate", `Basic realm="`+authRealm+`", charset="UTF-8"`)
	writeError(w, http.StatusUnauthorized, "unauthorized",
		"a valid admin token is required (Authorization: Bearer <token>, HTTP Basic password, or X-Admin-Token)")
}

// tokenEqual compares in constant time so a wrong token leaks no timing signal.
func tokenEqual(got, want string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// cutPrefixFold is strings.CutPrefix with a case-insensitive prefix match, so
// "bearer" and "Bearer" are both accepted.
func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):], true
	}
	return s, false
}
