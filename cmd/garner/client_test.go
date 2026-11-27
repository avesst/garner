package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// A flag a command does not use is a usage error, not silently dropped.
func TestUnusedFlagsRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/tokens" {
			io.WriteString(w, `{"token": {"name": "ci"}, "secret": "s"}`)
			return
		}
		io.WriteString(w, "null")
	}))
	defer srv.Close()
	deb := filepath.Join(t.TempDir(), "a.deb")
	os.WriteFile(deb, []byte("x"), 0o644)

	for _, tc := range []struct {
		args []string
		code int
	}{
		{[]string{"upload", "--version", "1", deb}, 2},
		{[]string{"upload", "--json", deb}, 2},
		{[]string{"packages", "list", "--suite", "stable"}, 2},
		{[]string{"sync", "src", "--all"}, 2},
		{[]string{"owner", "set", "a", "b", "--json"}, 2},
		{[]string{"snapshot", "rollback", "x", "--json"}, 2},
		{[]string{"publish", "--name", "x"}, 2},
		{[]string{"status", "--strict"}, 2},
		{[]string{"token", "list", "--name", "x"}, 2},

		{[]string{"upload", "--suite", "stable", "--release", "el9", deb}, 0},
		{[]string{"packages", "list", "--name", "a", "--distribution", "stable", "--all", "--json"}, 0},
		{[]string{"remove", "a", "--version", "1", "--arch", "amd64", "--distribution", "stable"}, 0},
		{[]string{"sync", "src", "--ignore-age", "v1", "--repo", "org/a"}, 0},
		{[]string{"owner", "list", "--json"}, 0},
		{[]string{"snapshot", "list", "--json"}, 0},
		{[]string{"conflicts", "list", "--all", "--json"}, 0},
		{[]string{"status", "--json"}, 0},
		{[]string{"token", "create", "--name", "ci", "--owner", "team", "--scopes", "upload"}, 0},
		{[]string{"token", "create", "--name", "ci", "--owner", "team", "--scope", "upload"}, 2},
		{[]string{"token", "list"}, 0},
	} {
		var out, errOut strings.Builder
		var code int
		if tc.args[0] == "token" {
			code = runAPIWith(srv.URL, "token", tc.args[1:], &out, &errOut)
		} else {
			code = runAPIWith(srv.URL, tc.args[0], tc.args[1:], &out, &errOut)
		}
		if code != tc.code {
			t.Errorf("garner %s: exit %d, want %d\n%s", strings.Join(tc.args, " "), code, tc.code, errOut.String())
		}
	}
}

// garner token passes only create's flags on to the API in --url mode.
func TestTokenURLModeFlags(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "null")
	}))
	defer srv.Close()
	for _, tc := range []struct {
		args []string
		code int
	}{
		{[]string{"list", "--url", srv.URL}, 0},
		{[]string{"revoke", "ci", "--url", srv.URL}, 0},
		{[]string{"list", "--url", srv.URL, "--owner", "x"}, 2},
		{[]string{"revoke", "ci", "--scopes", "upload", "--url", srv.URL}, 2},
	} {
		var out, errOut strings.Builder
		if code := runToken(tc.args, &out, &errOut); code != tc.code {
			t.Errorf("garner token %s: exit %d, want %d\n%s", strings.Join(tc.args, " "), code, tc.code, errOut.String())
		}
	}
}

// The filters and targets of the API reach it under their own names.
func TestFlagsReachAPI(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, r.Method+" "+r.URL.EscapedPath()+"?"+r.URL.RawQuery+" "+string(b))
		io.WriteString(w, "null")
	}))
	defer srv.Close()
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"packages", "list", "--format", "oci", "--arch", "amd64", "--state", "held"},
			"GET /api/v1/packages?arch=amd64&format=oci&state=held "},
		{[]string{"remove", "team/tool", "--format", "oci", "--version", "sha256:ab"},
			"DELETE /api/v1/packages/team%2Ftool?format=oci&version=sha256%3Aab "},
		{[]string{"owner", "set", "team/tool", "team-b", "--namespace", "oci"},
			`PUT /api/v1/owners/team%2Ftool? {"owner":"team-b","namespace":"oci"}`},
	} {
		got = nil
		var out, errOut strings.Builder
		if code := runAPIWith(srv.URL, tc.args[0], tc.args[1:], &out, &errOut); code != 0 {
			t.Fatalf("garner %s: exit %d\n%s", strings.Join(tc.args, " "), code, errOut.String())
		}
		if len(got) != 1 || strings.TrimSpace(got[0]) != strings.TrimSpace(tc.want) {
			t.Errorf("garner %s sent %q, want %q", strings.Join(tc.args, " "), got, tc.want)
		}
	}
	var out, errOut strings.Builder
	if code := runAPIWith(srv.URL, "owner", []string{"list", "--namespace", "oci"}, &out, &errOut); code != 2 {
		t.Errorf("owner list --namespace: exit %d, want 2", code)
	}
}

// Tables show the fields that tell rows apart or explain a missing asset.
func TestTableColumns(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/status":
			io.WriteString(w, `{"sources": [{"name": "org",
				"repos_without_assets": ["org/docs"], "skipped_assets": ["tool_1.0_s390x.deb"]},
				{"name": "quiet"}]}`)
		case "/api/v1/owners":
			io.WriteString(w, `[{"namespace": "oci", "name": "tool", "owner": "team-a"}]`)
		case "/api/v1/packages":
			io.WriteString(w, `[{"id": "p1", "name": "tool", "verify_status": "unverified"}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	for _, tc := range []struct {
		args []string
		want []string
		once []string
	}{
		{[]string{"status"}, []string{"WITHOUT ASSETS", "org/docs", "SKIPPED", "tool_1.0_s390x.deb"}, []string{"quiet"}},
		{[]string{"owner", "list"}, []string{"NAMESPACE", "oci"}, nil},
		{[]string{"packages", "list"}, []string{"VERIFY", "unverified"}, nil},
	} {
		var out, errOut strings.Builder
		if code := runAPIWith(srv.URL, tc.args[0], tc.args[1:], &out, &errOut); code != 0 {
			t.Fatalf("%v: exit %d: %s", tc.args, code, errOut.String())
		}
		for _, w := range tc.want {
			if !strings.Contains(out.String(), w) {
				t.Errorf("%v output lacks %q:\n%s", tc.args, w, out.String())
			}
		}
		// A source without items gets no rows in the extra lists.
		for _, o := range tc.once {
			if n := strings.Count(out.String(), o); n != 1 {
				t.Errorf("%v output has %q %d times, want 1:\n%s", tc.args, o, n, out.String())
			}
		}
	}
}
