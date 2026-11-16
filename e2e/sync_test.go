//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/avesst/garner/internal/deb/debtest"
	gsync "github.com/avesst/garner/internal/sync"
)

// fakeForgejo is a Forgejo API with one private repository, team/tool,
// holding one release with one .deb asset. It listens on two addresses,
// like a forge reached at an internal URL whose ROOT_URL (and so every
// browser_download_url) names another: api is the address garner is
// configured with, root the one asset links point to. Assets need the
// token, as for a private repository; without it the forge answers 404.
type fakeForgejo struct {
	api, root *httptest.Server
	token     string
	asset     []byte

	mu        sync.Mutex
	downloads int // asset requests, with or without the token
}

const fjAsset = "garner-tool_1.0_amd64.deb"

func newFakeForgejo(t *testing.T) *fakeForgejo {
	t.Helper()
	f := &fakeForgejo{token: "fj-secret", asset: debtest.Build("garner-tool", "1.0", "amd64", "tool 1.0")}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/repos/team/tool/releases", func(w http.ResponseWriter, r *http.Request) {
		rel := map[string]any{
			"id": 1, "tag_name": "v1.0", "draft": false, "prerelease": false,
			"published_at": time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
			"assets": []map[string]any{{
				"name": fjAsset, "size": len(f.asset),
				"browser_download_url": f.root.URL + "/team/tool/releases/download/v1.0/" + fjAsset,
			}},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]any{rel})
	})
	mux.HandleFunc("GET /team/tool/releases/download/v1.0/"+fjAsset, func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.downloads++
		f.mu.Unlock()
		if r.Header.Get("Authorization") != "token "+f.token {
			http.NotFound(w, r) // Forgejo hides private repositories
			return
		}
		w.Write(f.asset)
	})
	f.api = httptest.NewTLSServer(mux)
	f.root = httptest.NewTLSServer(mux)
	t.Cleanup(f.api.Close)
	t.Cleanup(f.root.Close)
	return f
}

func (f *fakeForgejo) downloadCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.downloads
}

// config writes the token and CA files into dir and returns the forges
// and sources sections, with the forge's url set to forgeURL.
func (f *fakeForgejo) config(t *testing.T, dir, forgeURL string) string {
	t.Helper()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.api.Certificate().Raw})
	if err := os.WriteFile(filepath.Join(dir, "forge-ca.pem"), ca, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "forge-token"), []byte(f.token), 0o600); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf(`forges:
  - name: fj
    type: forgejo
    url: %s
    token_file: %s/forge-token
    ca_file: %s/forge-ca.pem
sources:
  - name: tool
    forge: fj
    repo: team/tool
`, forgeURL, dir, dir)
}

// startSync attaches a sync manager to e the way serve does, with poll
// loops that never fire on their own: tests call runSync.
func startSync(t *testing.T, e *env) *gsync.Manager {
	t.Helper()
	m := &gsync.Manager{
		Store: e.app.Store, Ingester: e.app.Ingester, Formats: e.app.Formats, Config: e.app.Config,
		TmpDir: filepath.Join(e.dir, "tmp"), Log: e.app.Log, MaxStartDelay: 24 * time.Hour,
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); m.Stop() })
	return m
}

func runSync(t *testing.T, e *env, m *gsync.Manager) {
	t.Helper()
	src := e.app.Config().Sources[0]
	if !m.RunOnce(context.Background(), src) {
		t.Fatal("sync already running")
	}
}

// aptInstalls reports whether apt can install pkg from e and prints its
// payload.
func aptInstalls(e *env, pkg, payload string) (string, bool) {
	out, err := e.run("debian:trixie", aptSetup(e.url)+fmt.Sprintf(`
apt-get update -q
apt-get install -y -q %[1]s
cat /opt/%[1]s/payload
`, pkg))
	return out, err == nil && strings.Contains(out, payload)
}

// An ingest failure that is not the package's fault (here
// the blob store is unwritable, as with ENOSPC) must leave the release
// unseen, so the next poll delivers the package.
func TestSyncRetriesTransientIngestFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory mode this test relies on")
	}
	f := newFakeForgejo(t)
	dir := t.TempDir()
	e := newEnv(t, f.config(t, dir, f.api.URL))
	// Asset links on the configured origin get the token.
	f.root = f.api
	m := startSync(t, e)

	blobTmp := filepath.Join(e.dir, "blobs", "tmp")
	if err := os.Chmod(blobTmp, 0o555); err != nil {
		t.Fatal(err)
	}
	runSync(t, e, m)
	if err := os.Chmod(blobTmp, 0o755); err != nil {
		t.Fatal(err)
	}
	if n := f.downloadCount(); n != 1 {
		t.Fatalf("first sync downloaded the asset %d times, want 1", n)
	}

	runSync(t, e, m) // the disk is fine again
	e.publish()
	if out, ok := aptInstalls(e, "garner-tool", "tool 1.0"); !ok {
		t.Fatalf("package lost after a transient ingest failure (asset downloads: %d):\n%s", f.downloadCount(), out)
	}
}
