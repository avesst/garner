package app

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/avesst/garner/internal/auth"
	"github.com/avesst/garner/internal/config"
	"github.com/avesst/garner/internal/deb"
	"github.com/avesst/garner/internal/format"
	"github.com/avesst/garner/internal/signing"
)

// A reload applies log.format and log.level, also to loggers derived
// before it (sync derives one per source run).
func TestReloadAppliesLogSection(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "garner.yaml")
	write := func(format, level string) {
		t.Helper()
		yaml := fmt.Sprintf("server:\n  public_url: http://localhost\ndatabase:\n  url: sqlite://%s/garner.db\n"+
			"storage:\n  path: %s\nlog:\n  format: %s\n  level: %s\ndeb: {}\n", dir, dir, format, level)
		if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("json", "info")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := signing.GenerateBits(DefaultKeyPath(cfg), "t", "t@example.com", 2048); err != nil {
		t.Fatal(err)
	}

	// The logger writes to standard output.
	out, err := os.Create(filepath.Join(dir, "stdout"))
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = out
	t.Cleanup(func() { os.Stdout = stdout })

	a, err := Open(context.Background(), cfgPath, "test", debOnly)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	derived := a.Log.With("source", "s")
	grouped := derived.WithGroup("g")

	write("text", "warn")
	if err := a.Reload(); err != nil {
		t.Fatal(err)
	}
	derived.Info("hidden by the new level")
	derived.Warn("after reload")
	a.Log.Warn("base after reload")
	grouped.Warn("grouped", "k", 1)

	data, err := os.ReadFile(out.Name())
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if strings.Contains(got, "hidden by the new level") {
		t.Errorf("info record logged after reload to level warn:\n%s", got)
	}
	for _, want := range []string{`level=WARN msg="after reload" source=s`, `level=WARN msg="base after reload"`,
		`level=WARN msg=grouped source=s g.k=1`} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks text record %q:\n%s", want, got)
		}
	}
}

func debOnly(*config.Config) ([]format.Format, error) {
	return []format.Format{deb.Format{}}, nil
}

// openWith writes a configuration with the given format sections, creates
// a signing key and opens the app.
func openWith(t *testing.T, sections string, formats FormatsFunc) (*App, string, error) {
	t.Helper()
	return openFormatsIn(t, t.TempDir(), sections, formats)
}

// openIn is openWith for the deb format in dir.
func openIn(t *testing.T, dir, sections string) (*App, string, error) {
	t.Helper()
	return openFormatsIn(t, dir, sections, debOnly)
}

func openFormatsIn(t *testing.T, dir, sections string, formats FormatsFunc) (*App, string, error) {
	t.Helper()
	cfgPath := filepath.Join(dir, "garner.yaml")
	yaml := fmt.Sprintf("server:\n  public_url: http://localhost\n  listen: 127.0.0.1:0\ndatabase:\n  url: sqlite://%s/garner.db\n"+
		"storage:\n  path: %s\n%s", dir, dir, sections)
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := signing.GenerateBits(DefaultKeyPath(cfg), "t", "t@example.com", 2048); err != nil {
		t.Fatal(err)
	}
	a, err := Open(context.Background(), cfgPath, "test", formats)
	return a, cfgPath, err
}

// The registered formats must be exactly the configured sections.
func TestOpenRejectsFormatsThatDoNotMatchSections(t *testing.T) {
	none := func(*config.Config) ([]format.Format, error) { return nil, nil }
	if a, _, err := openWith(t, "deb: {}\n", none); err == nil {
		a.Close()
		t.Fatal("Open accepted a configured deb section without a deb format")
	} else if !strings.Contains(err.Error(), "do not match the configured sections") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// Formats are built once, so a reload that adds or removes a format
// section is rejected and the old configuration stays.
func TestReloadRejectsFormatSectionChange(t *testing.T) {
	a, cfgPath, err := openWith(t, "deb: {}\n", debOnly)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, append(data, "rpm: {}\n"...), 0o644); err != nil {
		t.Fatal(err)
	}
	err = a.Reload()
	if err == nil || !strings.Contains(err.Error(), "changing the format sections needs a restart") {
		t.Fatalf("reload error = %v, want a restart error", err)
	}
	if got := a.Config().Formats(); !slices.Equal(got, []string{"deb"}) {
		t.Errorf("formats after rejected reload = %v, want [deb]", got)
	}
}

// A key change while a format with re-signed packages is not configured
// must not be recorded, or those packages are never re-signed once its
// section is back. Unconfigured packages without a re-signed variant do
// not hold the key back.
func TestResignLeavesKeyUnrecordedForUnconfiguredFormat(t *testing.T) {
	a, _, err := openWith(t, "deb: {}\n", debOnly)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ctx := context.Background()
	if err := a.Store.SetSignedWith(ctx, "OLD"); err != nil {
		t.Fatal(err)
	}
	insert := func(id, fmtName, signed string) {
		t.Helper()
		sha := strings.Repeat(id[len(id)-1:], 64)
		for _, b := range []string{sha, signed} {
			if b == "" {
				continue
			}
			if err := a.Store.AddBlob(ctx, b, 1); err != nil {
				t.Fatal(err)
			}
		}
		if err := a.Store.InsertPackage(ctx, &format.Package{ID: id, Format: fmtName, Name: id,
			Version: "1", Release: "1", Arch: "noarch", BlobSHA256: sha, SignedSHA256: signed,
			Origin: "upload", OriginRef: "token:t", VerifyStatus: "n/a",
			CreatedAt: "2026-01-01T00:00:00Z"}); err != nil {
			t.Fatal(err)
		}
	}
	signedWith := func() string {
		t.Helper()
		fp, err := a.Store.SignedWith(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return fp
	}

	insert("p1", "rpm", strings.Repeat("f", 64))
	if err := a.resignIfNeeded(ctx); err != nil {
		t.Fatal(err)
	}
	if got := signedWith(); got != "OLD" {
		t.Errorf("signed_with = %q with re-signed rpm packages stored but rpm not configured, want OLD", got)
	}

	if _, err := a.DB.DB.ExecContext(ctx, `DELETE FROM packages WHERE id = 'p1'`); err != nil {
		t.Fatal(err)
	}
	insert("p2", "other", "")
	if err := a.resignIfNeeded(ctx); err != nil {
		t.Fatal(err)
	}
	if got := signedWith(); got != a.Signer().SigningFingerprint() {
		t.Errorf("signed_with = %q with only unsigned unconfigured packages, want the current key", got)
	}
}

// garner_signing_resign_pending reports a signing key that differs from
// the one recorded after the last re-sign.
// garner publish goes through the same path as a scheduled publish, so its
// duration, time and failures reach the metrics.
func TestAPIPublishRecordsMetrics(t *testing.T) {
	a, _, err := openWith(t, "deb: {}\n", debOnly)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	secret, _, err := auth.CreateToken(context.Background(), &a.Store.Queries, "pub", "team", []string{auth.ScopePublish})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/publish", nil)
	r.RemoteAddr = "127.0.0.1:5000"
	r.SetBasicAuth("pub", secret)
	w := httptest.NewRecorder()
	a.Server().Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/v1/publish = %d: %s", w.Code, w.Body)
	}
	var out strings.Builder
	a.Metrics.Write(&out)
	for _, name := range []string{"garner_publish_duration_seconds ", "garner_publish_last_success_timestamp_seconds "} {
		if !strings.Contains(out.String(), "\n"+name) {
			t.Errorf("metrics lack %s after a publish through the API", strings.TrimSpace(name))
		}
	}
}

func TestResignPendingMetric(t *testing.T) {
	a, _, err := openWith(t, "deb: {}\n", debOnly)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ctx := context.Background()
	for _, c := range []struct {
		recorded, want string
	}{
		{a.Signer().SigningFingerprint(), "garner_signing_resign_pending 0\n"},
		{"OLD", "garner_signing_resign_pending 1\n"},
	} {
		if err := a.Store.SetSignedWith(ctx, c.recorded); err != nil {
			t.Fatal(err)
		}
		var out strings.Builder
		a.Metrics.Write(&out)
		if !strings.Contains(out.String(), c.want) {
			t.Errorf("signed_with %s: metrics lack %q", c.recorded, strings.TrimSpace(c.want))
		}
	}
}
