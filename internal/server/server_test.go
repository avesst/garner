package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avesst/garner/internal/auth"
	"github.com/avesst/garner/internal/blob"
	"github.com/avesst/garner/internal/config"
	"github.com/avesst/garner/internal/db"
	"github.com/avesst/garner/internal/deb"
	"github.com/avesst/garner/internal/deb/debtest"
	"github.com/avesst/garner/internal/format"
	"github.com/avesst/garner/internal/ingest"
	"github.com/avesst/garner/internal/metrics"
	"github.com/avesst/garner/internal/publish"
	"github.com/avesst/garner/internal/signing"
	"github.com/avesst/garner/internal/store"
)

// noSources is a syncer without configured sources; like the real one, it
// reports them as a nil slice.
type noSources struct{}

func (noSources) Trigger(string) error                  { return nil }
func (noSources) Status(context.Context) []SourceStatus { return nil }

type fixture struct {
	cfg     *config.Config
	h       http.Handler
	st      *store.Store
	secrets map[string]string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	d, err := db.Open(context.Background(), filepath.Join(root, "garner.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	blobs, _ := blob.Open(filepath.Join(root, "blobs"))
	keyFile := filepath.Join(root, "key.asc")
	signing.GenerateBits(keyFile, "t", "t@example.com", 2048)
	keys, err := signing.Load([]signing.KeySource{{KeyFile: keyFile}})
	if err != nil {
		t.Fatal(err)
	}
	reg, _ := format.NewRegistry(deb.Format{})
	f := &fixture{cfg: config.Default(), secrets: map[string]string{}}
	f.cfg.Server.PublicURL = "http://repo.test"
	f.cfg.Server.MaxUploadSize = 64 << 10
	f.cfg.Server.TrustedProxies = []string{"10.0.0.1"}
	conf := func() *config.Config { return f.cfg }
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	f.st = store.New(d.DB)
	sched := &publish.Scheduler{}
	s := &Server{
		Store: f.st, Formats: reg, Config: conf, Metrics: metrics.NewRegistry(), Root: root, Log: log,
		Scheduler: sched, Syncer: noSources{},
		Ingester: &ingest.Ingester{Store: f.st, Blobs: blobs, Formats: reg, Config: conf, TmpDir: root,
			Publish: sched.Request, Log: log},
		Publisher: &publish.Publisher{Store: f.st, Blobs: blobs, Formats: reg, Config: conf,
			Signer: func() signing.Signer { return keys }, Root: root, Log: log},
	}
	f.h = s.Handler()
	for name, scopes := range map[string][]string{
		"up": {auth.ScopeUpload}, "pub": {auth.ScopePublish}, "adm": {auth.ScopeAdmin},
		"other": {auth.ScopeUpload},
	} {
		owner := "team"
		if name == "other" {
			owner = "other-team"
		}
		secret, _, err := auth.CreateToken(context.Background(), &f.st.Queries, name, owner, scopes)
		if err != nil {
			t.Fatal(err)
		}
		f.secrets[name] = secret
	}
	return f
}

// do sends a request from loopback unless remote is set.
func (f *fixture) do(method, target, token string, body []byte, remote string, header ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, bytes.NewReader(body))
	r.RemoteAddr = "127.0.0.1:5000"
	if remote != "" {
		r.RemoteAddr = remote
	}
	if token != "" {
		r.SetBasicAuth(token, f.secrets[token])
	}
	for i := 0; i+1 < len(header); i += 2 {
		r.Header.Set(header[i], header[i+1])
	}
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	return w
}

func TestUploadStatusCodes(t *testing.T) {
	f := newFixture(t)
	pkg := debtest.Build("app", "1.0", "amd64", "a")
	tests := []struct {
		name  string
		token string
		query string
		body  []byte
		want  int
	}{
		{"no credentials", "", "", pkg, http.StatusUnauthorized},
		{"wrong scope", "pub", "", pkg, http.StatusForbidden},
		{"ok", "up", "", pkg, http.StatusOK},
		{"identical again", "up", "?suite=stable", pkg, http.StatusOK},
		{"content conflict", "up", "", debtest.Build("app", "1.0", "amd64", "b"), http.StatusConflict},
		{"owned by another", "other", "", debtest.Build("app", "2.0", "amd64", "a"), http.StatusConflict},
		{"unknown suite", "up", "?suite=jammy", pkg, http.StatusBadRequest},
		{"not a package", "up", "", []byte("hello"), http.StatusBadRequest},
		{"too large", "up", "", make([]byte, 65<<10), http.StatusRequestEntityTooLarge},
	}
	for _, tt := range tests {
		w := f.do(http.MethodPut, "/api/v1/upload"+tt.query, tt.token, tt.body, "")
		if w.Code != tt.want {
			t.Errorf("%s: HTTP %d, want %d: %s", tt.name, w.Code, tt.want, w.Body)
		}
	}
	w := f.do(http.MethodPut, "/api/v1/upload?suite=jammy", "up", pkg, "")
	var body errorBody
	json.Unmarshal(w.Body.Bytes(), &body)
	if len(body.Configured["suite"]) == 0 {
		t.Errorf("400 for an unknown suite does not list configured suites: %s", w.Body)
	}
}

func TestCredentialsNeedSecureTransport(t *testing.T) {
	f := newFixture(t)
	f.cfg.Server.TrustedProxies = []string{"10.0.0.1"}
	pkg := debtest.Build("app", "1.0", "amd64", "a")
	tests := []struct {
		name   string
		remote string
		header []string
		want   int
	}{
		{"plain http from the network", "192.0.2.7:1000", nil, http.StatusForbidden},
		{"untrusted proxy claiming https", "192.0.2.7:1000", []string{"X-Forwarded-Proto", "https"}, http.StatusForbidden},
		{"trusted proxy over http", "10.0.0.1:1000", []string{"X-Forwarded-Proto", "http"}, http.StatusForbidden},
		{"trusted proxy over https", "10.0.0.1:1000", []string{"X-Forwarded-Proto", "https"}, http.StatusOK},
		{"ipv6 loopback", "[::1]:1000", nil, http.StatusOK},
		{"same-host proxy over https", "127.0.0.1:1000", []string{"X-Forwarded-Proto", "https"}, http.StatusOK},
		{"same-host proxy over http", "127.0.0.1:1000", []string{"X-Forwarded-Proto", "http"}, http.StatusForbidden},
		{"proxy chain with an http hop", "10.0.0.1:1000", []string{"X-Forwarded-Proto", "https, http"}, http.StatusForbidden},
	}
	for _, tt := range tests {
		w := f.do(http.MethodPut, "/api/v1/upload", "up", pkg, tt.remote, tt.header...)
		if w.Code != tt.want {
			t.Errorf("%s: HTTP %d, want %d: %s", tt.name, w.Code, tt.want, w.Body)
		}
	}
	f.cfg.Server.AllowInsecureAuth = true
	if w := f.do(http.MethodPut, "/api/v1/upload", "up", pkg, "192.0.2.7:1000"); w.Code != http.StatusOK {
		t.Errorf("allow_insecure_auth: HTTP %d", w.Code)
	}
}

func TestPublishRemoveAndRead(t *testing.T) {
	f := newFixture(t)
	if w := f.do(http.MethodPut, "/api/v1/upload", "up", debtest.Build("app", "1.0", "amd64", "a"), ""); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if w := f.do(http.MethodPost, "/api/v1/publish", "pub", nil, ""); w.Code != 200 {
		t.Fatalf("publish: %d %s", w.Code, w.Body)
	}
	w := f.do(http.MethodGet, "/deb/dists/stable/main/binary-amd64/Packages", "", nil, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Package: app") {
		t.Fatalf("Packages: %d %s", w.Code, w.Body)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Packages Cache-Control = %q", cc)
	}
	if w := f.do(http.MethodPost, "/deb/dists/stable/InRelease", "", nil, ""); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST to the tree: HTTP %d", w.Code)
	}
	if w := f.do(http.MethodDelete, "/api/v1/packages/app", "pub", nil, ""); w.Code != http.StatusForbidden {
		t.Errorf("remove with publish scope: HTTP %d", w.Code)
	}
	if w := f.do(http.MethodDelete, "/api/v1/packages/app?version=1.0", "adm", nil, ""); w.Code != 200 {
		t.Errorf("remove: HTTP %d %s", w.Code, w.Body)
	}
	w = f.do(http.MethodGet, "/api/v1/status", "", nil, "")
	var st Status
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil || st.CurrentSnapshot == "" {
		t.Errorf("status: %v %s", err, w.Body)
	}
	if !strings.Contains(w.Body.String(), `"sources": []`) {
		t.Errorf("status without sources should list [] , got:\n%s", w.Body)
	}
	if w := f.do(http.MethodGet, "/api/v1/nope", "", nil, ""); w.Code != http.StatusNotFound {
		t.Errorf("unknown API path: HTTP %d", w.Code)
	}
	if w := f.do(http.MethodGet, "/metrics", "", nil, ""); !strings.Contains(w.Body.String(), `garner_uploads_total{result="ok"} 1`) {
		t.Errorf("metrics:\n%s", w.Body)
	}
}

func TestTrustedProxyPrefix(t *testing.T) {
	pol := auth.Policy{TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "[::ffff:10.1.2.3]:80"
	if !auth.FromTrustedProxy(pol, r) {
		t.Error("IPv4-mapped IPv6 address not matched against an IPv4 prefix")
	}
}
