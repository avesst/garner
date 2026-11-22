//go:build e2e

// Package e2e runs real apt and dnf clients in containers against garner.
// Run with: go test -tags e2e ./e2e (needs Docker and network access to
// pull images).
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
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
	"time"

	"github.com/avesst/garner/internal/app"
	"github.com/avesst/garner/internal/auth"
	"github.com/avesst/garner/internal/config"
	"github.com/avesst/garner/internal/deb"
	"github.com/avesst/garner/internal/format"
	"github.com/avesst/garner/internal/publish"
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

func newEnv(t *testing.T, extraConfig string) *env {
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
	a, err := app.Open(context.Background(), cfgPath, "e2e", func(*config.Config) ([]format.Format, error) {
		return []format.Format{deb.Format{}}, nil
	})
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
	verifyAfter(t, url, "ci", secret)
	return e
}

// verifyAfter runs garner verify through the API when the test ends, before
// the server stops, so every scenario also checks the whole published tree.
// Errors fail the test; warnings are logged. It skips when garner is not
// running, as in tests where it must refuse to start.
func verifyAfter(t *testing.T, url, user, secret string) {
	t.Cleanup(func() {
		if resp, err := http.Get(url + "/readyz"); err != nil {
			t.Logf("verify skipped: garner is not running: %v", err)
			return
		} else if resp.Body.Close(); resp.StatusCode != http.StatusOK {
			t.Logf("verify skipped: garner is not ready (HTTP %d)", resp.StatusCode)
			return
		}
		// ready=1 waits out a publish the scheduler has not run yet.
		var body []byte
		for i := 0; ; i++ {
			req, _ := http.NewRequest(http.MethodPost, url+"/api/v1/verify?ready=1", nil)
			req.SetBasicAuth(user, secret)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Errorf("verify: %v", err)
				return
			}
			body, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusConflict && i < 150 {
				time.Sleep(200 * time.Millisecond)
				continue
			}
			if resp.StatusCode != http.StatusOK {
				t.Errorf("verify: HTTP %d: %s", resp.StatusCode, body)
				return
			}
			break
		}
		var rep publish.VerifyReport
		if err := json.Unmarshal(body, &rep); err != nil {
			t.Errorf("verify: %v: %s", err, body)
			return
		}
		for _, p := range rep.Problems {
			if p.Severity == format.SeverityError {
				t.Errorf("verify: %s %s: %s", p.Check, p.Path, p.Message)
			} else {
				t.Logf("verify warning: %s %s: %s", p.Check, p.Path, p.Message)
			}
		}
		t.Logf("verify: snapshot %s, %d packages, %d errors, %d warnings", rep.Snapshot, rep.Packages, rep.Errors, rep.Warnings)
	})
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
