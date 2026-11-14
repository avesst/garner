//go:build e2e

// Package e2e runs real apt and dnf clients in containers against garner.
// Run with: go test -tags e2e ./e2e (needs Docker and network access to
// pull images).
package e2e

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/avesst/garner/internal/app"
	"github.com/avesst/garner/internal/auth"
	"github.com/avesst/garner/internal/format"
	"github.com/avesst/garner/internal/signing"
)

// env is a running garner instance on a loopback port, reachable from
// containers on the host network.
type env struct {
	t      *testing.T
	dir    string
	app    *app.App
	url    string
	token  string
	srv    *httptest.Server
	mu     sync.Mutex
	onFile map[string]func() // path suffix -> hook run once before serving
}

func newEnv(t *testing.T, extraConfig string, formats ...format.Format) *env {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed")
	}
	dir := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	url := "http://" + ln.Addr().String()
	cfgPath := filepath.Join(dir, "garner.yaml")
	cfg := fmt.Sprintf(`server:
  public_url: %s
log:
  format: text
  level: warn
database:
  url: sqlite://%s/garner.db
storage:
  path: %s
deb:
  architectures: [amd64]
  suites: [stable]
%s`, url, dir, dir, extraConfig)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "signing", "key.asc")
	if _, err := signing.Generate(keyPath, "e2e repository", ""); err != nil {
		t.Fatal(err)
	}
	a, err := app.Open(context.Background(), cfgPath, "e2e", formats...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	secret, _, err := auth.CreateToken(context.Background(), &a.Store.Queries, "ci", "team", auth.AllScopes)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, dir: dir, app: a, url: url, token: secret, onFile: map[string]func(){}}
	h := a.Server().Handler()
	e.srv = &httptest.Server{Listener: ln, Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Render the response first, then run the hook: the client gets
		// the file as it was before the hook changed the repository.
		e.mu.Lock()
		var hook func()
		for suffix, fn := range e.onFile {
			if strings.HasSuffix(r.URL.Path, suffix) {
				hook = fn
				delete(e.onFile, suffix)
			}
		}
		e.mu.Unlock()
		if hook == nil {
			h.ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		hook()
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		w.Write(rec.Body.Bytes())
	})}}
	e.srv.Start()
	t.Cleanup(e.srv.Close)
	return e
}

// hook runs fn once, after the response for a path ending in suffix has
// been rendered and before it is sent.
func (e *env) hook(suffix string, fn func()) {
	e.mu.Lock()
	e.onFile[suffix] = fn
	e.mu.Unlock()
}

func (e *env) upload(data []byte, query string) {
	e.t.Helper()
	req, _ := http.NewRequest(http.MethodPut, e.url+"/api/v1/upload?"+query, bytes.NewReader(data))
	req.SetBasicAuth("ci", e.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("upload: HTTP %d: %s", resp.StatusCode, body)
	}
}

func (e *env) publish() {
	e.t.Helper()
	if _, err := e.app.Publisher.Publish(context.Background()); err != nil {
		e.t.Fatal(err)
	}
}

// run runs a shell script in a container on the host network and returns
// its combined output.
func (e *env) run(image, script string) (string, error) {
	e.t.Helper()
	cmd := exec.Command("docker", "run", "--rm", "--network", "host", image, "sh", "-ec", script)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
