package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// recorder answers every request with 200 {} and keeps the requests.
type recorder struct{ reqs []*http.Request }

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.reqs = append(r.reqs, req)
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}")), Request: req}, nil
}

// The token is sent in clear over plain HTTP only to loopback, or when
// GARNER_INSECURE_AUTH=1 says so, mirroring server.allow_insecure_auth.
func TestTokenNotSentInClear(t *testing.T) {
	t.Setenv("GARNER_TOKEN", "ci:secret")
	for _, tc := range []struct {
		base     string
		insecure string
		wantAuth bool
	}{
		{"https://repo.example", "", true},
		{"http://localhost:8080", "", true},
		{"http://127.0.0.1:8080", "", true},
		{"http://[::1]:8080", "", true},
		{"http://repo.example:8080", "", false},
		{"http://10.0.0.5:8080", "", false},
		{"http://repo.example:8080", "1", true},
	} {
		t.Setenv("GARNER_INSECURE_AUTH", tc.insecure)
		c, err := newClient(tc.base)
		if err != nil {
			t.Fatal(err)
		}
		rec := &recorder{}
		c.http.Transport = rec
		err = c.do(http.MethodGet, "/api/v1/status", nil, nil, nil)
		sent := len(rec.reqs) > 0 && rec.reqs[0].Header.Get("Authorization") != ""
		if sent != tc.wantAuth {
			t.Errorf("%s (insecure=%q): Authorization sent = %v, want %v", tc.base, tc.insecure, sent, tc.wantAuth)
		}
		if !tc.wantAuth && (err == nil || !strings.Contains(err.Error(), "GARNER_INSECURE_AUTH")) {
			t.Errorf("%s: err = %v, want a refusal naming GARNER_INSECURE_AUTH", tc.base, err)
		}
	}
}

func TestSyncIgnoreAgeAndDeferredStatus(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/sources/org/sync":
			b, _ := io.ReadAll(r.Body)
			body = string(b)
			io.WriteString(w, `{"status": "sync started", "ignore_age": "org/lib@v2"}`)
		case "/api/v1/status":
			io.WriteString(w, `{"sources": [{"name": "org", "deferred_releases": [
				{"repo": "org/lib", "tag": "v3", "published_at": "2026-11-20T00:00:00.000000Z",
				 "eligible_at": "2026-11-23T00:00:00.000000Z"},
				{"repo": "org/app", "tag": "v1"}]}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runAPIWith(srv.URL, "sync", []string{"org", "--ignore-age", "v2", "--repo", "org/lib"}, &out, &errOut); code != 0 {
		t.Fatalf("sync exit %d: %s", code, errOut.String())
	}
	if body != `{"ignore_age":{"tag":"v2","repo":"org/lib"}}` {
		t.Errorf("request body = %s", body)
	}
	if !strings.Contains(out.String(), "org/lib@v2 may skip min_release_age") {
		t.Errorf("output = %q", out.String())
	}
	if code := runAPIWith(srv.URL, "sync", []string{"org", "--repo", "org/lib"}, &out, &errOut); code != 2 {
		t.Errorf("--repo without --ignore-age: exit %d, want 2", code)
	}

	out.Reset()
	if code := runAPIWith(srv.URL, "status", nil, &out, &errOut); code != 0 {
		t.Fatalf("status exit %d: %s", code, errOut.String())
	}
	for _, want := range []string{"org/lib@v3", "2026-11-23T00:00:00.000000Z", "org/app@v1", "only by --ignore-age"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status output lacks %q:\n%s", want, out.String())
		}
	}
}
