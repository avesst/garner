package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/avesst/garner/internal/deb/debtest"
	"github.com/avesst/garner/internal/publish"
)

func TestVerifyEndpoint(t *testing.T) {
	f := newFixture(t)
	f.cfg.Hash = "0123456789abcdef0123456789abcdef"
	report := func(t *testing.T, target string) publish.VerifyReport {
		t.Helper()
		w := f.do(http.MethodPost, target, "pub", nil, "")
		if w.Code != http.StatusOK {
			t.Fatalf("%s: HTTP %d %s", target, w.Code, w.Body)
		}
		var rep publish.VerifyReport
		if err := json.Unmarshal(w.Body.Bytes(), &rep); err != nil {
			t.Fatal(err)
		}
		return rep
	}

	if w := f.do(http.MethodPost, "/api/v1/verify", "", nil, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("without a token: HTTP %d", w.Code)
	}
	if w := f.do(http.MethodPost, "/api/v1/verify", "up", nil, ""); w.Code != http.StatusForbidden {
		t.Errorf("upload scope: HTTP %d", w.Code)
	}
	if rep := report(t, "/api/v1/verify"); rep.Errors != 1 || rep.Snapshot != "" {
		t.Errorf("before the first publish: %+v", rep)
	}

	if w := f.do(http.MethodPut, "/api/v1/upload", "up", debtest.Build("app", "1.0", "amd64", "a"), ""); w.Code != 200 {
		t.Fatal(w.Body)
	}
	// The test scheduler does not publish, so the upload stays pending.
	if w := f.do(http.MethodPost, "/api/v1/verify?ready=1", "pub", nil, ""); w.Code != http.StatusConflict {
		t.Errorf("ready with a pending publish: HTTP %d %s", w.Code, w.Body)
	}
	if w := f.do(http.MethodPost, "/api/v1/publish", "pub", nil, ""); w.Code != 200 {
		t.Fatalf("publish: %d %s", w.Code, w.Body)
	}
	if rep := report(t, "/api/v1/verify?ready=1&config_hash=0123456789AB"); rep.Errors != 0 || rep.Warnings != 0 ||
		rep.Packages != 1 || rep.PublishPending || rep.ConfigHash != f.cfg.Hash {
		t.Errorf("after publish: %+v", rep)
	}

	for target, code := range map[string]int{
		"/api/v1/verify?config_hash=fedcba9876": http.StatusConflict,
		"/api/v1/verify?config_hash=0123":       http.StatusBadRequest,
	} {
		if w := f.do(http.MethodPost, target, "pub", nil, ""); w.Code != code {
			t.Errorf("%s: HTTP %d, want %d: %s", target, w.Code, code, w.Body)
		}
	}
}
