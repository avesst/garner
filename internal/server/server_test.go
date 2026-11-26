package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
// It records the overrides of min_release_age it is given; tests can set
// its status and make Trigger fail.
type noSources struct {
	ignored    []string
	status     []SourceStatus
	triggerErr error
}

func (n *noSources) Trigger(string) error                  { return n.triggerErr }
func (n *noSources) Status(context.Context) []SourceStatus { return n.status }
func (n *noSources) IgnoreAge(_ context.Context, source, repo, tag, by string) error {
	n.ignored = append(n.ignored, source+" "+repo+"@"+tag+" by "+by)
	return nil
}

type fixture struct {
	cfg     *config.Config
	h       http.Handler
	st      *store.Store
	in      *ingest.Ingester
	sync    *noSources
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
	f := &fixture{cfg: config.Default(), sync: &noSources{}, secrets: map[string]string{}}
	f.cfg.Server.PublicURL = "http://repo.test"
	f.cfg.Storage.MaxPackageSize = 64 << 10
	f.cfg.Server.TrustedProxies = []string{"10.0.0.1"}
	conf := func() *config.Config { return f.cfg }
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	f.st = store.New(d.DB)
	sched := &publish.Scheduler{}
	s := &Server{
		Store: f.st, Formats: reg, Config: conf, Metrics: metrics.NewRegistry(), Root: root, Log: log,
		Scheduler: sched, Syncer: f.sync,
		Ingester: &ingest.Ingester{Store: f.st, Blobs: blobs, Formats: reg, Config: conf, TmpDir: root,
			Publish: sched.Request, Log: log},
		Publisher: &publish.Publisher{Store: f.st, Blobs: blobs, Formats: reg, Config: conf,
			Signer: func() signing.Signer { return keys }, Root: root, Log: log},
	}
	f.h = s.Handler()
	f.in = s.Ingester
	for name, scopes := range map[string][]string{
		"up": {auth.ScopeUpload}, "pub": {auth.ScopePublish}, "adm": {auth.ScopeAdmin},
		"pubadm": {auth.ScopePublish, auth.ScopeAdmin},
		"other":  {auth.ScopeUpload},
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
		// apt rejects the whole Packages file over one such field.
		{"malformed Depends", "up", "", debtest.BuildWith("app", "3.0", "amd64", "a", "Depends: libc6 (>= 2.31\n"), http.StatusBadRequest},
		// Its pool file would not fit a path component, failing every publish.
		{"version too long", "up", "", debtest.Build("app", "4."+strings.Repeat("0", 250), "amd64", "a"), http.StatusBadRequest},
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
	if _, rest, ok := strings.Cut(w.Body.String(), "Filename: "); ok {
		file, _, _ := strings.Cut(rest, "\n")
		w := f.do(http.MethodGet, "/deb/"+file, "", nil, "")
		if cc := w.Header().Get("Cache-Control"); w.Code != 200 || !strings.Contains(cc, "immutable") {
			t.Errorf("pool file %s: HTTP %d, Cache-Control = %q", file, w.Code, cc)
		}
	} else {
		t.Error("Packages has no Filename")
	}
	for _, path := range []string{"/deb/pool/", "/snapshots/"} {
		w := f.do(http.MethodGet, path, "", nil, "")
		if cc := w.Header().Get("Cache-Control"); w.Code != 200 || cc != "no-cache" {
			t.Errorf("listing %s: HTTP %d, Cache-Control = %q", path, w.Code, cc)
		}
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

// Approving a package held for an ownership conflict must not put a second
// file of a version into a distribution where the owner has delivered it.
func TestApproveRechecksContent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if w := f.do(http.MethodPut, "/api/v1/upload", "up", debtest.Build("app", "1.0", "amd64", "a"), ""); w.Code != http.StatusOK {
		t.Fatalf("upload: HTTP %d: %s", w.Code, w.Body)
	}
	foreign, err := f.in.Ingest(ctx, bytes.NewReader(debtest.Build("app", "2.0", "amd64", "squat")), ingest.Request{
		Owner: ingest.SourceOwner("other"), Origin: "source", OriginRef: "x", VerifyStatus: "unverified",
		HoldOnOwnerConflict: true,
	})
	if err != nil || foreign.State != store.Held {
		t.Fatalf("foreign sync = %+v, %v; want held", foreign, err)
	}
	if w := f.do(http.MethodPut, "/api/v1/upload", "up", debtest.Build("app", "2.0", "amd64", "real"), ""); w.Code != http.StatusOK {
		t.Fatalf("owner's upload: HTTP %d: %s", w.Code, w.Body)
	}

	w := f.do(http.MethodPost, "/api/v1/packages/"+foreign.Package.ID+"/approve", "pubadm", nil, "")
	if w.Code != http.StatusConflict {
		t.Errorf("approve: HTTP %d, want 409: %s", w.Code, w.Body)
	}
	held, err := f.st.Placed(ctx, store.PackageFilter{ID: foreign.Package.ID, States: []string{store.Held}})
	if err != nil || len(held) != 1 {
		t.Errorf("foreign package after a refused approve = %v, %v; want still held", held, err)
	}
}

// Approving a package held because its deliverer does not own the name
// publishes it under another owner's name, so it needs the admin scope as
// well (section 8).
func TestApproveOwnershipHoldNeedsAdmin(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if w := f.do(http.MethodPut, "/api/v1/upload", "up", debtest.Build("app", "1.0", "amd64", "a"), ""); w.Code != http.StatusOK {
		t.Fatalf("upload: HTTP %d: %s", w.Code, w.Body)
	}
	foreign, err := f.in.Ingest(ctx, bytes.NewReader(debtest.Build("app", "2.0", "amd64", "squat")), ingest.Request{
		Owner: ingest.SourceOwner("other"), Origin: "source", OriginRef: "x", VerifyStatus: "unverified",
		HoldOnOwnerConflict: true,
	})
	if err != nil || foreign.State != store.Held {
		t.Fatalf("foreign sync = %+v, %v; want held", foreign, err)
	}
	// A hold for approval by the name's owner needs publish alone.
	own, err := f.in.Ingest(ctx, bytes.NewReader(debtest.Build("app", "3.0", "amd64", "a")), ingest.Request{
		Owner: "team", Origin: "upload", OriginRef: "x", VerifyStatus: "n/a", Held: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	approve := func(id, token string) int {
		return f.do(http.MethodPost, "/api/v1/packages/"+id+"/approve", token, nil, "").Code
	}
	if code := approve(foreign.Package.ID, "pub"); code != http.StatusForbidden {
		t.Errorf("approve of an ownership hold with publish alone: HTTP %d, want 403", code)
	}
	if held, _ := f.st.Placed(ctx, store.PackageFilter{ID: foreign.Package.ID, States: []string{store.Held}}); len(held) != 1 {
		t.Errorf("ownership hold after a refused approve = %v; want still held", held)
	}
	if code := approve(foreign.Package.ID, "pubadm"); code != http.StatusOK {
		t.Errorf("approve of an ownership hold with publish and admin: HTTP %d, want 200", code)
	}
	if code := approve(own.Package.ID, "pub"); code != http.StatusOK {
		t.Errorf("approve of the owner's hold with publish: HTTP %d, want 200", code)
	}
}

// Skipping min_release_age needs admin on top of publish, and a repository
// whenever the source has more than one.
func TestSyncIgnoreAge(t *testing.T) {
	f := newFixture(t)
	off := time.Duration(0)
	f.cfg.Defaults.MinReleaseAge = 72 * time.Hour
	f.cfg.Sources = []config.Source{{Name: "tool", Repo: "org/tool"}, {Name: "org", Org: "org"},
		{Name: "unlisted", Org: "other"}, {Name: "ungated", Repo: "org/free", MinReleaseAge: &off}}
	if err := f.st.SaveRepos(context.Background(), "org", []string{"org/lib", "org/app"}); err != nil {
		t.Fatal(err)
	}
	for tag, id := range map[string]string{"v0": "100", "v5": "105"} {
		if err := f.st.MarkSeen(context.Background(), "tool", "org/tool", tag, id, time.Time{}); err != nil {
			t.Fatal(err)
		}
	}
	// v5 was processed, then deleted and re-created upstream: the new
	// release waits.
	f.sync.status = []SourceStatus{{Name: "tool", Deferred: []DeferredRelease{{Repo: "org/tool", Tag: "v5"}}}}
	body := func(tag, repo string) []byte {
		b, _ := json.Marshal(SyncRequest{IgnoreAge: &IgnoreAgeRequest{Tag: tag, Repo: repo}})
		return b
	}
	tests := []struct {
		name, source, token string
		body                []byte
		code                int
	}{
		{"plain sync", "tool", "pub", nil, http.StatusAccepted},
		{"publish only", "tool", "pub", body("v1", ""), http.StatusForbidden},
		{"admin only", "tool", "adm", body("v1", ""), http.StatusForbidden},
		{"single repo", "tool", "pubadm", body("v1", ""), http.StatusAccepted},
		{"wrong repo", "tool", "pubadm", body("v1", "org/other"), http.StatusBadRequest},
		{"org without repo", "org", "pubadm", body("v2", ""), http.StatusBadRequest},
		{"org with repo", "org", "pubadm", body("v2", "org/lib"), http.StatusAccepted},
		{"repo in other case", "org", "pubadm", body("v3", "Org/App"), http.StatusAccepted},
		{"single repo in other case", "tool", "pubadm", body("v4", "Org/Tool"), http.StatusAccepted},
		{"unknown repo", "org", "pubadm", body("v2", "org/nope"), http.StatusBadRequest},
		{"already processed", "tool", "pubadm", body("v0", ""), http.StatusConflict},
		{"source without the gate", "ungated", "pubadm", body("v1", ""), http.StatusConflict},
		{"re-created after processing", "tool", "pubadm", body("v5", ""), http.StatusAccepted},
		{"source never listed", "unlisted", "pubadm", body("v2", "other/x"), http.StatusBadRequest},
		{"no tag", "tool", "pubadm", body("", ""), http.StatusBadRequest},
		{"unknown source", "nope", "pubadm", body("v1", ""), http.StatusNotFound},
		{"bad body", "tool", "pubadm", []byte("{"), http.StatusBadRequest},
		{"misspelled key", "tool", "pubadm", []byte(`{"ignoreAge": {"tag": "v1"}}`), http.StatusBadRequest},
		{"trailing data", "tool", "pubadm", append(body("v1", ""), []byte(` {}`)...), http.StatusBadRequest},
		{"empty object", "tool", "pub", []byte(`{}`), http.StatusAccepted},
	}
	for _, tt := range tests {
		w := f.do(http.MethodPost, "/api/v1/sources/"+tt.source+"/sync", tt.token, tt.body, "")
		if w.Code != tt.code {
			t.Errorf("%s: HTTP %d, want %d: %s", tt.name, w.Code, tt.code, w.Body)
		}
	}
	want := "tool org/tool@v1 by pubadm; org org/lib@v2 by pubadm; org org/app@v3 by pubadm; tool org/tool@v4 by pubadm; tool org/tool@v5 by pubadm"
	if got := strings.Join(f.sync.ignored, "; "); got != want {
		t.Errorf("overrides = %q, want %q", got, want)
	}

	// The override is stored even when the sync cannot start, so the
	// answer must not read as a failure.
	f.sync.triggerErr = errors.New(`no source named "tool"`)
	w := f.do(http.MethodPost, "/api/v1/sources/tool/sync", "pubadm", body("v6", ""), "")
	if w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), "override recorded; sync not started") {
		t.Errorf("trigger failure: HTTP %d: %s", w.Code, w.Body)
	}
}
