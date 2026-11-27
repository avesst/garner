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
					"size": 13, "digest": "sha256:ABCD", "created_at": "2026-11-02T00:00:00Z",
					"updated_at": "2026-11-05T00:00:00Z"}]}]`, base)
				return
			}
			io.WriteString(w, `[{"id": 1, "tag_name": "v1", "prerelease": true, "published_at": "2026-11-01T00:00:00Z", "assets": []}]`)
		case "/api/v3/repos/org/tool/releases/2":
			io.WriteString(w, `{"id": 2, "tag_name": "v2", "published_at": "2026-11-02T00:00:00Z", "assets": []}`)
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
	// A rename moves updated_at, and can turn an old file into a candidate.
	if a := rels[0].Assets[0]; !rels[0].AssetTimes || !a.CreatedAt.Equal(time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("asset time = %v (dated %v), want updated_at", a.CreatedAt, rels[0].AssetTimes)
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

	if rel, found, err := c.Release(ctx, "org/tool", "2"); err != nil || !found || rel.Tag != "v2" {
		t.Errorf("release 2 by ID = %+v, %v, %v", rel, found, err)
	}
	if _, found, err := c.Release(ctx, "org/tool", "99"); err != nil || found {
		t.Errorf("deleted release by ID: found %v, err %v; want not found and no error", found, err)
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
			if r.URL.Query().Get("with_shared") != "false" {
				http.Error(w, "want with_shared=false", 400)
				return
			}
			// A project shared into the group from another namespace, as a
			// GitLab that ignores with_shared would still list it.
			io.WriteString(w, `[{"path_with_namespace": "platform/tools/cli", "forked_from_project": {"id": 1}},
				{"path_with_namespace": "other-team/tool"},
				{"path_with_namespace": "platform/tools-old/x"}]`)
		case "/api/v4/projects/platform%2Ftools%2Fcli/releases/v1":
			io.WriteString(w, `{"tag_name": "v1", "released_at": "2026-10-01T00:00:00Z", "created_at": "2026-11-01T00:00:00Z"}`)
		case "/api/v4/projects/platform%2Ftools%2Fcli/releases":
			// released_at order (the default) places an upcoming release
			// where it comes out.
			if r.URL.Query().Has("order_by") {
				http.Error(w, "want the default order", 400)
				return
			}
			// A historical release: released_at backdated before creation.
			fmt.Fprintf(w, `[{"tag_name": "v1", "released_at": "2026-10-01T00:00:00Z", "created_at": "2026-11-01T00:00:00Z",
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
	if err != nil || len(repos) != 1 || repos[0].Path != "platform/tools/cli" || !repos[0].Fork {
		t.Fatalf("repos = %+v, %v; want only platform/tools/cli, not projects outside the group", repos, err)
	}
	rels, _, err := c.Releases(ctx, repos[0].Path, time.Time{}, "")
	// The ID adds the creation time, so a release re-created under the
	// same tag is a new release.
	if err != nil || len(rels) != 1 || rels[0].ID != "v1:2026-11-01T00:00:00Z" || rels[0].Tag != "v1" || rels[0].FormerID != "v1" {
		t.Fatalf("releases = %+v, %v", rels, err)
	}
	if rel, found, err := c.Release(ctx, repos[0].Path, rels[0].ID); err != nil || !found || rel.ID != rels[0].ID {
		t.Errorf("release by ID = %+v, %v, %v", rel, found, err)
	}
	if _, found, err := c.Release(ctx, repos[0].Path, "v1:2026-10-15T00:00:00Z"); err != nil || found {
		t.Errorf("re-created release by old ID: found %v, err %v; want not found", found, err)
	}
	// A bare tag is an ID from before IDs carried the creation time.
	if rel, found, err := c.Release(ctx, repos[0].Path, "v1"); err != nil || !found || rel.ID != rels[0].ID {
		t.Errorf("release by bare tag = %+v, %v, %v; want it under its current ID", rel, found, err)
	}
	if _, found, err := c.Release(ctx, repos[0].Path, "v9:2026-11-01T00:00:00Z"); err != nil || found {
		t.Errorf("deleted release by ID: found %v, err %v; want not found", found, err)
	}
	if !rels[0].PublishedAt.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) || rels[0].AssetTimes {
		t.Errorf("published %v (assets dated %v), want created_at and undated links", rels[0].PublishedAt, rels[0].AssetTimes)
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

	// The same 404 from outside the forge's origin was fetched without the
	// token, so it may only mean "private": not permanent.
	other := httptest.NewTLSServer(http.NotFoundHandler())
	defer other.Close()
	_, err = c.Download(context.Background(), Asset{Name: "x", URL: other.URL + "/x"})
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 404 || !se.External {
		t.Fatalf("external download: err = %v, want an external 404", err)
	}
	if se.Permanent() {
		t.Error("an unauthenticated 404 from outside the forge counts as permanent")
	}
}

func TestForgejoAssetTimes(t *testing.T) {
	f, _ := tlsForge(t, "forgejo", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/repos/org/tool/releases/7" {
			io.WriteString(w, `{"id": 7, "tag_name": "v1", "published_at": "2026-11-01T00:00:00Z", "assets": []}`)
			return
		}
		if r.URL.Path != "/api/v1/repos/org/tool/releases" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `[{"id": 7, "tag_name": "v1", "published_at": "2026-11-01T00:00:00Z",
			"assets": [{"name": "tool.deb", "size": 3, "browser_download_url": "https://x/tool.deb",
			"created_at": "2026-11-04T00:00:00Z"}]}]`)
	}))
	c, err := New(f)
	if err != nil {
		t.Fatal(err)
	}
	rels, _, err := c.Releases(context.Background(), "org/tool", time.Time{}, "")
	if err != nil || len(rels) != 1 || len(rels[0].Assets) != 1 {
		t.Fatalf("releases = %+v, %v", rels, err)
	}
	if a := rels[0].Assets[0]; !rels[0].AssetTimes || !a.CreatedAt.Equal(time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("asset time = %v (dated %v), want created_at", a.CreatedAt, rels[0].AssetTimes)
	}
	if rel, found, err := c.Release(context.Background(), "org/tool", "7"); err != nil || !found || rel.Tag != "v1" {
		t.Errorf("release 7 by ID = %+v, %v, %v", rel, found, err)
	}
	if _, found, err := c.Release(context.Background(), "org/tool", "8"); err != nil || found {
		t.Errorf("deleted release by ID: found %v, err %v", found, err)
	}
}

// Without recursive, a subgroup's project is not the group's, even if
// GitLab lists it.
func TestGitLabNonRecursiveKeepsDirectProjects(t *testing.T) {
	f, _ := tlsForge(t, "gitlab", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("include_subgroups") != "false" {
			http.Error(w, "want no subgroups", 400)
			return
		}
		io.WriteString(w, `[{"path_with_namespace": "Platform/cli"}, {"path_with_namespace": "platform/sub/x"}]`)
	}))
	c, err := New(f)
	if err != nil {
		t.Fatal(err)
	}
	repos, err := c.ListRepos(context.Background(), "platform", false)
	if err != nil || len(repos) != 1 || repos[0].Path != "Platform/cli" {
		t.Fatalf("repos = %+v, %v; want only Platform/cli", repos, err)
	}
}
