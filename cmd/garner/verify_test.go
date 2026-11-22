package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestVerifyWaitsAndExitCodes(t *testing.T) {
	old := verifyRetry
	verifyRetry = 10 * time.Millisecond
	t.Cleanup(func() { verifyRetry = old })

	pending := 0
	body := `{"snapshot": "01S", "snapshot_config_hash": "abcdef0123456789", "packages": 2, "problems": [
		{"severity": "warning", "check": "garbage", "path": "pool/deb", "message": "3 pool files"}], "warnings": 1}`
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/verify" {
			http.NotFound(w, r)
			return
		}
		queries = append(queries, r.URL.RawQuery)
		if pending > 0 && r.URL.Query().Get("ready") == "1" {
			pending--
			w.WriteHeader(http.StatusConflict)
			io.WriteString(w, `{"error": "a publish is pending"}`)
			return
		}
		io.WriteString(w, body)
	}))
	defer srv.Close()

	var out, errOut strings.Builder
	pending = 2
	if code := runAPIWith(srv.URL, "verify", []string{"--config-hash", "abcdef0"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if len(queries) != 3 || queries[0] != "config_hash=abcdef0&ready=1" {
		t.Errorf("queries %q, want three with config_hash and ready", queries)
	}
	for _, want := range []string{"snapshot 01S (configuration abcdef012345), 2 packages", "warning", "pool/deb: 3 pool files", "0 errors, 1 warning"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}

	if code := runAPIWith(srv.URL, "verify", []string{"--strict"}, &out, &errOut); code != 1 {
		t.Errorf("--strict with a warning: exit %d, want 1", code)
	}

	body = `{"snapshot": "01S", "problems": [{"severity": "error", "check": "pool", "message": "missing"}], "errors": 1}`
	if code := runAPIWith(srv.URL, "verify", nil, &out, &errOut); code != 1 {
		t.Errorf("with an error: exit %d, want 1", code)
	}

	pending = 1000
	errOut.Reset()
	if code := runAPIWith(srv.URL, "verify", []string{"--wait", "50ms"}, &out, &errOut); code != 1 ||
		!strings.Contains(errOut.String(), "gave up after 50ms: a publish is pending") {
		t.Errorf("waiting past the deadline: exit %d, stderr %q", code, errOut.String())
	}
}
