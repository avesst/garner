package forge

import (
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/avesst/garner/internal/config"
)

// tlsForge starts a TLS test server and returns a forge config that
// trusts it through ca_file, as an internal forge would be set up.
func tlsForge(t *testing.T, typ string, h http.Handler) (config.Forge, *httptest.Server) {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	pemData := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(ca, pemData, 0o644); err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tokenFile, []byte("s3cret\n"), 0o600)
	return config.Forge{Name: "f", Type: typ, URL: srv.URL, CAFile: ca, TokenFile: tokenFile, MaxConcurrent: 2}, srv
}

// recorder notes the credentials each path was requested with.
type recorder struct {
	mu   sync.Mutex
	auth map[string]string
}

func (r *recorder) note(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.auth == nil {
		r.auth = map[string]string{}
	}
	r.auth[req.URL.Path] = req.Header.Get("Authorization") + req.Header.Get("PRIVATE-TOKEN")
}

func (r *recorder) get(path string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.auth[path]
}

func TestGitHubEnterpriseReleasesAndDownload(t *testing.T) {
	var rec recorder
	// The storage host the asset redirects to: it must get no token.
	storage := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.note(r)
		io.WriteString(w, "package bytes")
	}))
	defer storage.Close()

	var base string
	f, _ := tlsForge(t, "github", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.note(r)
		switch r.URL.Path {
		case "/api/v3/repos/org/tool/releases":
			if r.Header.Get("If-None-Match") == `"v1"` {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", `"v1"`)
			w.Header().Set("X-RateLimit-Remaining", "4999")
			if r.URL.Query().Get("page") == "" {
				w.Header().Set("Link", fmt.Sprintf(`<%s/api/v3/repos/org/tool/releases?page=2>; rel="next"`, base))
				fmt.Fprintf(w, `[{"id": 2, "tag_name": "v2", "published_at": "2026-11-02T00:00:00Z",
					"assets": [{"name": "tool_2_amd64.deb", "url": "%s/api/v3/repos/org/tool/releases/assets/9",
					"size": 13, "digest": "sha256:ABCD"}]}]`, base)
				return
			}
			io.WriteString(w, `[{"id": 1, "tag_name": "v1", "prerelease": true, "published_at": "2026-11-01T00:00:00Z", "assets": []}]`)
		case "/api/v3/repos/org/tool/releases/assets/9":
			if r.Header.Get("Accept") != "application/octet-stream" {
				http.Error(w, "want octet-stream", 400)
				return
			}
			http.Redirect(w, r, storage.URL+"/blob", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	ca, _ := os.ReadFile(f.CAFile)
	os.WriteFile(f.CAFile, append(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: storage.Certificate().Raw})...), 0o644)
	base = f.URL
	c, err := New(f)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	rels, etag, err := c.Releases(ctx, "org/tool", time.Time{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rels) != 2 || rels[0].Tag != "v2" || !rels[1].Prerelease || etag != `"v1"` {
		t.Fatalf("releases = %+v, etag %q", rels, etag)
	}
	if a := rels[0].Assets[0]; a.SHA256 != "ABCD" || a.Size != 13 {
		t.Errorf("asset = %+v", a)
	}
	if c.RateLimitRemaining() != 4999 {
		t.Errorf("rate limit = %d", c.RateLimitRemaining())
	}
	if _, _, err := c.Releases(ctx, "org/tool", time.Time{}, `"v1"`); !errors.Is(err, ErrNotModified) {
		t.Errorf("with matching ETag: err = %v, want ErrNotModified", err)
	}
	// Stop paging once a page reaches releases at or before since.
	rels, _, _ = c.Releases(ctx, "org/tool", time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC), "")
	if len(rels) != 1 {
		t.Errorf("with since after v2: %d releases, want 1 page", len(rels))
	}

	body, err := c.Download(ctx, rels[0].Assets[0])
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(body)
	body.Close()
	if string(data) != "package bytes" {
		t.Errorf("downloaded %q", data)
	}
	if got := rec.get("/api/v3/repos/org/tool/releases/assets/9"); got != "Bearer s3cret" {
		t.Errorf("API asset request auth = %q", got)
	}
	if got := rec.get("/blob"); got != "" {
		t.Errorf("redirect to the storage host carried credentials %q", got)
	}
}

func TestForgejoListReposPaginates(t *testing.T) {
	var base string
	f, _ := tlsForge(t, "forgejo", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token s3cret" {
			http.Error(w, "auth", 401)
			return
		}
		if r.URL.Path != "/api/v1/orgs/platform/repos" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("page") == "2" {
			io.WriteString(w, `[{"full_name": "platform/b", "archived": true}]`)
			return
		}
		w.Header().Set("Link", fmt.Sprintf(`<%s/api/v1/orgs/platform/repos?page=2&limit=50>; rel="next"`, base))
		io.WriteString(w, `[{"full_name": "platform/a", "fork": true}]`)
	}))
	base = f.URL
	c, err := New(f)
	if err != nil {
		t.Fatal(err)
	}
	repos, err := c.ListRepos(context.Background(), "platform", false)
	if err != nil {
		t.Fatal(err)
	}
	want := []Repo{{Path: "platform/a", Fork: true}, {Path: "platform/b", Archived: true}}
	if fmt.Sprint(repos) != fmt.Sprint(want) {
		t.Errorf("repos = %+v, want %+v", repos, want)
	}
}

func TestGitLabEscapesPathsAndExternalLinks(t *testing.T) {
	var rec recorder
	external := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.note(r)
		io.WriteString(w, "x")
	}))
	defer external.Close()
	f, _ := tlsForge(t, "gitlab", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.note(r)
		switch r.URL.RawPath {
		case "/api/v4/groups/platform%2Ftools/projects":
			if r.URL.Query().Get("include_subgroups") != "true" {
				http.Error(w, "want subgroups", 400)
				return
			}
			io.WriteString(w, `[{"path_with_namespace": "platform/tools/cli", "forked_from_project": {"id": 1}}]`)
		case "/api/v4/projects/platform%2Ftools%2Fcli/releases":
			fmt.Fprintf(w, `[{"tag_name": "v1", "released_at": "2026-11-01T00:00:00Z",
				"assets": {"links": [{"name": "cli.rpm", "url": "%s/cli.rpm"}]}}]`, external.URL)
		default:
			http.Error(w, "unexpected "+r.URL.RawPath, 404)
		}
	}))
	ca, _ := os.ReadFile(f.CAFile)
	os.WriteFile(f.CAFile, append(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: external.Certificate().Raw})...), 0o644)
	c, err := New(f)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	repos, err := c.ListRepos(ctx, "platform/tools", true)
	if err != nil || len(repos) != 1 || !repos[0].Fork {
		t.Fatalf("repos = %+v, %v", repos, err)
	}
	rels, _, err := c.Releases(ctx, repos[0].Path, time.Time{}, "")
	if err != nil || len(rels) != 1 || rels[0].ID != "v1" {
		t.Fatalf("releases = %+v, %v", rels, err)
	}
	body, err := c.Download(ctx, rels[0].Assets[0])
	if err != nil {
		t.Fatal(err)
	}
	body.Close()
	if got := rec.get("/cli.rpm"); got != "" {
		t.Errorf("external link got credentials %q", got)
	}
	if got := rec.get("/api/v4/projects/platform/tools/cli/releases"); got != "s3cret" {
		t.Errorf("API request PRIVATE-TOKEN = %q", got)
	}
}

func TestUntrustedCertificateFails(t *testing.T) {
	f, _ := tlsForge(t, "forgejo", http.NotFoundHandler())
	f.CAFile = ""
	c, err := New(f)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.ListRepos(context.Background(), "x", false)
	if err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Errorf("err = %v, want a certificate error", err)
	}
}

func TestDownloadStatusError(t *testing.T) {
	codes := map[string]int{"/gone": 404, "/removed": 410, "/busy": 503, "/denied": 403}
	f, srv := tlsForge(t, "forgejo", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(codes[r.URL.Path])
	}))
	c, err := New(f)
	if err != nil {
		t.Fatal(err)
	}
	for p, code := range codes {
		_, err := c.Download(context.Background(), Asset{Name: p, URL: srv.URL + p})
		var se *StatusError
		if !errors.As(err, &se) || se.Code != code {
			t.Errorf("%s: err = %v, want a StatusError with %d", p, err, code)
			continue
		}
		if want := code == 404 || code == 410; se.Permanent() != want {
			t.Errorf("%s: Permanent() = %v, want %v", p, se.Permanent(), want)
		}
	}
}
