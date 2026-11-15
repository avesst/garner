// Package server is garner's HTTP server: the public tree, the API, health
// endpoints and metrics (design sections 4, 13 and 14).
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avesst/garner/internal/auth"
	"github.com/avesst/garner/internal/config"
	"github.com/avesst/garner/internal/format"
	"github.com/avesst/garner/internal/ingest"
	"github.com/avesst/garner/internal/metrics"
	"github.com/avesst/garner/internal/publish"
	"github.com/avesst/garner/internal/store"
)

// Syncer triggers forge syncs.
type Syncer interface {
	// Trigger starts a sync of the named source; it returns an error if
	// the source does not exist.
	Trigger(name string) error
	// Status returns per-source state for /api/v1/status.
	Status(ctx context.Context) []SourceStatus
}

// SourceStatus is one source's sync state.
type SourceStatus struct {
	Name        string   `json:"name"`
	LastRun     string   `json:"last_run,omitempty"`
	LastSuccess string   `json:"last_success,omitempty"`
	LastError   string   `json:"last_error,omitempty"`
	Unmatched   []string `json:"repos_without_assets,omitempty"`
	// Skipped lists assets of the last run whose distribution or
	// architecture is not configured.
	Skipped []string `json:"skipped_assets,omitempty"`
}

// Server serves everything on one handler.
type Server struct {
	Store     *store.Store
	Formats   *format.Registry
	Ingester  *ingest.Ingester
	Publisher *publish.Publisher
	Scheduler *publish.Scheduler
	Syncer    Syncer
	Config    func() *config.Config
	// ConfigCommit returns the git commit of the configuration, or "".
	ConfigCommit func() string
	Reload       func() error
	Metrics      *metrics.Registry
	Root         string
	Version      string
	Log          *slog.Logger

	// MetricsSeparate disables /metrics on the main handler.
	MetricsSeparate bool

	httpRequests *metrics.Vec
	uploads      *metrics.Vec
	uploadBytes  *metrics.Vec
}

// Handler builds the HTTP handler.
func (s *Server) Handler() http.Handler {
	s.httpRequests = s.Metrics.Counter("garner_http_requests_total", "HTTP requests by route and status code.", "route", "code")
	s.uploads = s.Metrics.Counter("garner_uploads_total", "Uploads by result.", "result")
	s.uploadBytes = s.Metrics.Counter("garner_upload_bytes_total", "Bytes of accepted uploads.")

	mux := http.NewServeMux()
	handle := func(pattern string, h http.HandlerFunc) {
		mux.Handle(pattern, s.instrument(pattern, h))
	}
	handle("GET /healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	handle("GET /readyz", s.readyz)
	if !s.MetricsSeparate {
		mux.Handle("GET /metrics", s.Metrics.Handler())
	}

	handle("PUT /api/v1/upload", s.authed(auth.ScopeUpload, s.upload))
	handle("GET /api/v1/status", s.status)
	handle("GET /api/v1/packages", s.listPackages)
	handle("DELETE /api/v1/packages/{name}", s.authed(auth.ScopeAdmin, s.removePackage))
	handle("POST /api/v1/packages/{id}/approve", s.authed(auth.ScopePublish, s.approve))
	handle("GET /api/v1/owners", s.listOwners)
	handle("PUT /api/v1/owners/{name}", s.authed(auth.ScopeAdmin, s.setOwner))
	handle("POST /api/v1/sources/{name}/sync", s.authed(auth.ScopePublish, s.sync))
	handle("GET /api/v1/conflicts", s.authed(auth.ScopePublish, s.listConflicts))
	handle("GET /api/v1/snapshots", s.listSnapshots)
	handle("POST /api/v1/snapshots/{id}/rollback", s.authed(auth.ScopePublish, s.rollback))
	handle("POST /api/v1/publish", s.authed(auth.ScopePublish, s.publishNow))
	handle("POST /api/v1/reload", s.authed(auth.ScopeAdmin, s.reload))
	handle("POST /api/v1/gc", s.authed(auth.ScopeAdmin, s.gc))
	handle("GET /api/v1/tokens", s.authed(auth.ScopeAdmin, s.listTokens))
	handle("POST /api/v1/tokens", s.authed(auth.ScopeAdmin, s.createToken))
	handle("DELETE /api/v1/tokens/{name}", s.authed(auth.ScopeAdmin, s.revokeToken))
	handle("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "no such API endpoint")
	})

	files := http.FileServer(http.Dir(filepath.Join(s.Root, "public")))
	// "/" without a method: "GET /" would overlap "/api/" ambiguously.
	handle("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeError(w, http.StatusMethodNotAllowed, "the repository is read-only")
			return
		}
		setCache(w, r)
		files.ServeHTTP(w, r)
	})
	return mux
}

// setCache lets proxies keep content-addressed files and makes them
// revalidate everything else, which changes with each publish.
//
// Directory listings change with each upload or publish, so paths ending
// in "/" always revalidate, even inside the pool and snapshots.
//
// For the mutable files it also drops If-Modified-Since: Last-Modified has
// one-second resolution, so after two publishes within a second a client
// would get 304 for a changed InRelease.
func setCache(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if !strings.HasSuffix(path, "/") && (strings.Contains(path, "/by-hash/") ||
		strings.Contains(path, "/pool/") || strings.HasPrefix(path, "/snapshots/")) {
		w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	r.Header.Del("If-Modified-Since")
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

func (s *Server) instrument(route string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		h.ServeHTTP(rec, r)
		s.httpRequests.Inc(route, fmt.Sprint(rec.code))
	})
}

type tokenKey struct{}

func tokenFrom(r *http.Request) *store.Token {
	t, _ := r.Context().Value(tokenKey{}).(*store.Token)
	return t
}

func (s *Server) policy() auth.Policy {
	cfg := s.Config()
	return auth.Policy{TrustedProxies: cfg.TrustedProxyPrefixes(), AllowInsecureAuth: cfg.Server.AllowInsecureAuth}
}

func (s *Server) authed(scope string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t, err := auth.Authenticate(r.Context(), &s.Store.Queries, s.policy(), r)
		switch {
		case errors.Is(err, auth.ErrNoCredentials), errors.Is(err, auth.ErrBadToken):
			w.Header().Set("WWW-Authenticate", `Basic realm="garner"`)
			writeError(w, http.StatusUnauthorized, err.Error())
			return
		case errors.Is(err, auth.ErrInsecure):
			writeError(w, http.StatusForbidden, err.Error())
			return
		case err != nil:
			s.internal(w, err)
			return
		}
		if !auth.HasScope(t, scope) {
			writeError(w, http.StatusForbidden, fmt.Sprintf("token %q lacks the %s scope", t.Name, scope))
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), tokenKey{}, t)))
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

// errorBody is every API error response.
type errorBody struct {
	Error string `json:"error"`
	// Configured lists valid distributions on a 400 for an unknown one.
	Configured map[string][]string `json:"configured,omitempty"`
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, errorBody{Error: msg})
}

func (s *Server) internal(w http.ResponseWriter, err error) {
	s.Log.Error("request failed", "err", err)
	writeError(w, http.StatusInternalServerError, "internal error; see the server log")
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var problems []string
	if err := s.Store.Ping(ctx); err != nil {
		problems = append(problems, "database: "+err.Error())
	}
	if _, err := os.Stat(filepath.Join(s.Root, "blobs")); err != nil {
		problems = append(problems, "storage: "+err.Error())
	}
	if _, err := os.Stat(filepath.Join(s.Root, "public", "current")); err != nil {
		problems = append(problems, "no current snapshot")
	}
	if len(problems) > 0 {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintln(w, strings.Join(problems, "\n"))
		return
	}
	fmt.Fprintln(w, "ready")
}
