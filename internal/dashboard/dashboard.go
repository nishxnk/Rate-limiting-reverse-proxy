// Package dashboard serves the embedded single-page admin UI.
//
// The page is compiled into the binary with go:embed, so the container ships
// one static artifact with no runtime asset dependency and no CDN calls.
package dashboard

import (
	"bytes"
	"embed"
	"net/http"
	"strings"
	"time"
)

//go:embed static/index.html
var files embed.FS

// Path is where the dashboard is mounted.
const Path = "/dashboard"

// Handler serves the dashboard page.
func Handler() (http.Handler, error) {
	page, err := files.ReadFile("static/index.html")
	if err != nil {
		return nil, err
	}
	modTime := time.Now()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Everything under /dashboard renders the same single page.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Referrer-Policy", "same-origin")
		// The page is fully self contained: no external scripts, styles or
		// fonts, so it can be locked down hard.
		w.Header().Set("Content-Security-Policy",
			"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; img-src data:; base-uri 'none'; form-action 'none'")
		http.ServeContent(w, r, "index.html", modTime, bytes.NewReader(page))
	}), nil
}

// Register mounts the dashboard under prefix. Both the bare and trailing-slash
// forms are served; the page derives its own API base from location.pathname,
// so either works.
func Register(mux *http.ServeMux, prefix string, h http.Handler) {
	p := strings.TrimSuffix(prefix, "/")
	mux.Handle("GET "+p+Path, h)
	mux.Handle("GET "+p+Path+"/", h)
}
