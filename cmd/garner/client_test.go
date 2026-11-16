package main

import (
	"io"
	"net/http"
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
