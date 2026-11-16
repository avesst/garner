package ingest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/avesst/garner/internal/blob"
	"github.com/avesst/garner/internal/config"
	"github.com/avesst/garner/internal/db"
	"github.com/avesst/garner/internal/deb"
	"github.com/avesst/garner/internal/deb/debtest"
	"github.com/avesst/garner/internal/format"
	"github.com/avesst/garner/internal/store"
)

type fixture struct {
	in        *Ingester
	cfg       *config.Config
	publishes int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Open(context.Background(), filepath.Join(dir, "garner.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	blobs, err := blob.Open(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	reg, _ := format.NewRegistry(deb.Format{})
	cfg := config.Default()
	cfg.Deb.Suites = []string{"stable", "noble"}
	cfg.Sources = []config.Source{{Name: "tool", Owns: []string{"tool-*"}}}
	f := &fixture{cfg: cfg}
	f.in = &Ingester{
		Store: store.New(d.DB), Blobs: blobs, Formats: reg,
		Config: func() *config.Config { return f.cfg }, TmpDir: dir,
		Publish: func() { f.publishes++ },
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return f
}

func (f *fixture) upload(t *testing.T, data []byte, dist, owner string) (*Result, error) {
	t.Helper()
	return f.in.Ingest(context.Background(), bytes.NewReader(data), Request{
		Targets: map[string]string{"suite": dist}, Owner: owner, Origin: "upload", OriginRef: "test", VerifyStatus: "n/a",
	})
}

func wantKind(t *testing.T, err error, kind Kind) {
	t.Helper()
	var ie *Error
	if !errors.As(err, &ie) || ie.Kind != kind {
		t.Fatalf("err = %v, want ingest error of kind %d", err, kind)
	}
}

func TestIngestAndReupload(t *testing.T) {
	f := newFixture(t)
	pkg := debtest.Build("app", "1:1.0-1", "amd64", "a")
	res, err := f.upload(t, pkg, "", "team")
	if err != nil {
		t.Fatal(err)
	}
	if res.Existing || res.State != store.Active || res.Distribution != "stable" {
		t.Errorf("first upload = %+v", res)
	}
	if res.Package.Epoch != 1 || res.Package.Version != "1.0-1" {
		t.Errorf("stored version %d:%s, want 1:1.0-1", res.Package.Epoch, res.Package.Version)
	}
	if f.publishes != 1 {
		t.Errorf("publishes = %d, want 1", f.publishes)
	}
	res, err = f.upload(t, pkg, "stable", "team")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Existing || f.publishes != 1 {
		t.Errorf("identical re-upload: Existing = %v, publishes = %d", res.Existing, f.publishes)
	}
}

func TestContentConflict(t *testing.T) {
	f := newFixture(t)
	if _, err := f.upload(t, debtest.Build("app", "1.0", "amd64", "a"), "stable", "team"); err != nil {
		t.Fatal(err)
	}
	_, err := f.upload(t, debtest.Build("app", "1.0", "amd64", "b"), "stable", "team")
	wantKind(t, err, Conflict)

	// Different content in another suite is allowed (section 7).
	res, err := f.upload(t, debtest.Build("app", "1.0", "amd64", "b"), "noble", "team")
	if err != nil {
		t.Fatal(err)
	}
	// The same file in a second suite reuses the package row.
	first, err := f.upload(t, debtest.Build("app", "1.0", "amd64", "a"), "noble", "team")
	if err == nil {
		t.Fatalf("noble already has app 1.0 with other content; got %+v", first)
	}
	again, err := f.upload(t, debtest.Build("app", "1.0", "amd64", "b"), "noble", "team")
	if err != nil || again.Package.ID != res.Package.ID {
		t.Errorf("re-upload to noble: %v, id %v want %v", err, again, res.Package.ID)
	}
}

func TestSamePackageInTwoSuitesSharesRow(t *testing.T) {
	f := newFixture(t)
	pkg := debtest.Build("app", "1.0", "amd64", "a")
	a, err := f.upload(t, pkg, "stable", "team")
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.upload(t, pkg, "noble", "team")
	if err != nil {
		t.Fatal(err)
	}
	if a.Package.ID != b.Package.ID || b.Existing {
		t.Errorf("second suite: id %s vs %s, Existing %v", a.Package.ID, b.Package.ID, b.Existing)
	}
}

func TestOwnership(t *testing.T) {
	f := newFixture(t)
	if _, err := f.upload(t, debtest.Build("app", "1.0", "amd64", "a"), "", "team-a"); err != nil {
		t.Fatal(err)
	}
	_, err := f.upload(t, debtest.Build("app", "2.0", "amd64", "a"), "", "team-b")
	wantKind(t, err, Conflict)

	// Reserved by source "tool": uploads are rejected, the source may deliver.
	_, err = f.upload(t, debtest.Build("tool-cli", "1.0", "amd64", "a"), "", "team-a")
	wantKind(t, err, Conflict)
	if _, err := f.upload(t, debtest.Build("tool-cli", "1.0", "amd64", "a"), "", SourceOwner("tool")); err != nil {
		t.Fatal(err)
	}

	// Sync of a foreign name is stored held, with a conflict.
	res, err := f.in.Ingest(context.Background(), bytes.NewReader(debtest.Build("app", "3.0", "amd64", "a")), Request{
		Owner: SourceOwner("other"), Origin: "source", OriginRef: "x", VerifyStatus: "unverified",
		HoldOnOwnerConflict: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.State != store.Held || res.Conflict == "" {
		t.Errorf("foreign sync = %+v, want held with a conflict", res)
	}
	cs, err := f.in.Store.Conflicts(context.Background(), false)
	if err != nil || len(cs) != 1 {
		t.Errorf("conflicts = %v, %v; want 1", cs, err)
	}
}

func TestRejectsUnconfigured(t *testing.T) {
	f := newFixture(t)
	_, err := f.upload(t, debtest.Build("app", "1.0", "arm64", "a"), "", "team")
	wantKind(t, err, NotConfigured)
	_, err = f.upload(t, debtest.Build("app", "1.0", "amd64", "a"), "jammy", "team")
	wantKind(t, err, NotConfigured)
	_, err = f.upload(t, []byte("not a package"), "", "team")
	wantKind(t, err, Invalid)
	if _, err := f.upload(t, debtest.Build("app", "1.0", "all", "a"), "", "team"); err != nil {
		t.Errorf("Architecture: all rejected: %v", err)
	}
}

// Without a target, a package goes to the default distribution: stable
// if configured, else the first one listed, as for the client files.
func TestDefaultDistribution(t *testing.T) {
	for _, tc := range []struct {
		suites []string
		want   string
	}{
		{[]string{"noble", "stable"}, "stable"},
		{[]string{"noble", "jammy"}, "noble"},
	} {
		f := newFixture(t)
		f.cfg.Deb.Suites = tc.suites
		res, err := f.upload(t, debtest.Build("app", "1.0", "amd64", "a"), "", "team")
		if err != nil {
			t.Fatalf("suites %v: %v", tc.suites, err)
		}
		if res.Distribution != tc.want {
			t.Errorf("suites %v: distribution %q, want %q", tc.suites, res.Distribution, tc.want)
		}
	}
}
