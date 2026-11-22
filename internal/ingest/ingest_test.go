package ingest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
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

// A package that replaces another owner's package on clients (deb
// Replaces, rpm Obsoletes) is refused like an upload of that name.
func TestOwnershipOfReplacedNames(t *testing.T) {
	f := newFixture(t)
	if _, err := f.upload(t, debtest.Build("victim", "1.0", "amd64", "b"), "", "team-b"); err != nil {
		t.Fatal(err)
	}
	_, err := f.upload(t, debtest.BuildWith("mine", "1.0", "amd64", "a", "Replaces: victim (<< 2.0)\n"), "", "team-a")
	wantKind(t, err, Conflict)
	if !strings.Contains(err.Error(), `"victim"`) || !strings.Contains(err.Error(), "team-b") {
		t.Errorf("err = %v, want it to name victim and its owner", err)
	}
	pkgs, _ := f.in.Store.Placed(context.Background(), store.PackageFilter{Name: "mine"})
	if len(pkgs) != 0 {
		t.Errorf("refused package was stored: %+v", pkgs)
	}
	if _, err := f.in.Store.Owner(context.Background(), "os", "mine"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("refused package claimed its name: %v", err)
	}
}

func TestOwnershipOfReplacedNamesAllowedAndClaimed(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// A rename within one owner.
	if _, err := f.upload(t, debtest.Build("old-name", "1.0", "amd64", "a"), "", "team-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.upload(t, debtest.BuildWith("new-name", "1.0", "amd64", "a", "Replaces: old-name\n"), "", "team-a"); err != nil {
		t.Fatalf("replacing an own package: %v", err)
	}

	// A free name is claimed, so its later upload by someone else fails.
	if _, err := f.upload(t, debtest.BuildWith("mine", "1.0", "amd64", "a", "Replaces: victim\n"), "", "team-a"); err != nil {
		t.Fatal(err)
	}
	if o, err := f.in.Store.Owner(ctx, "os", "victim"); err != nil || o.Owner != "team-a" {
		t.Errorf("owner of victim = %+v, %v; want team-a", o, err)
	}
	_, err := f.upload(t, debtest.Build("victim", "1.0", "amd64", "b"), "", "team-b")
	wantKind(t, err, Conflict)

	// A sync is stored held, with a conflict.
	res, err := f.in.Ingest(ctx, bytes.NewReader(debtest.BuildWith("synced", "1.0", "amd64", "s", "Replaces: mine\n")), Request{
		Owner: SourceOwner("other"), Origin: "source", OriginRef: "x", VerifyStatus: "unverified",
		HoldOnOwnerConflict: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.State != store.Held || !strings.Contains(res.Conflict, `"mine"`) {
		t.Errorf("sync replacing a foreign name = %+v, want held with a conflict naming mine", res)
	}
}

// A delivery held for an ownership conflict claims none of its names:
// otherwise a refused package would lock out the owners-to-be of every
// free name it lists.
func TestOwnershipHoldClaimsNoNames(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.upload(t, debtest.Build("owned", "1.0", "amd64", "b"), "", "team-b"); err != nil {
		t.Fatal(err)
	}
	res, err := f.in.Ingest(ctx, bytes.NewReader(debtest.BuildWith("squat", "1.0", "amd64", "s", "Replaces: free1, owned\n")), Request{
		Owner: SourceOwner("other"), Origin: "source", OriginRef: "x", VerifyStatus: "unverified",
		HoldOnOwnerConflict: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.State != store.Held {
		t.Fatalf("state = %s, want held", res.State)
	}
	for _, name := range []string{"squat", "free1"} {
		if o, err := f.in.Store.Owner(ctx, "os", name); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("held delivery claimed %q: %+v, %v", name, o, err)
		}
	}
	if _, err := f.upload(t, debtest.Build("free1", "1.0", "amd64", "c"), "", "team-c"); err != nil {
		t.Errorf("upload of a name the held package listed: %v", err)
	}
}

// A package held because its deliverer does not own the name does not
// stop the owner from delivering that version; a package held for
// approval still does.
func TestOwnershipHoldDoesNotBlockOwner(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.upload(t, debtest.Build("victim", "1.0", "amd64", "b"), "", "team-b"); err != nil {
		t.Fatal(err)
	}
	res, err := f.in.Ingest(ctx, bytes.NewReader(debtest.Build("victim", "2.0", "amd64", "squat")), Request{
		Owner: SourceOwner("other"), Origin: "source", OriginRef: "x", VerifyStatus: "unverified",
		HoldOnOwnerConflict: true,
	})
	if err != nil || res.State != store.Held {
		t.Fatalf("foreign sync = %+v, %v; want held", res, err)
	}
	res, err = f.upload(t, debtest.Build("victim", "2.0", "amd64", "real"), "", "team-b")
	if err != nil {
		t.Fatalf("owner's upload after a foreign hold: %v", err)
	}
	if res.State != store.Active {
		t.Errorf("owner's upload state = %s, want active", res.State)
	}

	// The owner's own hold for approval still decides the version.
	res, err = f.in.Ingest(ctx, bytes.NewReader(debtest.Build("victim", "3.0", "amd64", "first")), Request{
		Owner: "team-b", Origin: "upload", OriginRef: "x", VerifyStatus: "n/a", Held: true,
	})
	if err != nil || res.State != store.Held {
		t.Fatalf("held for approval = %+v, %v; want held", res, err)
	}
	_, err = f.upload(t, debtest.Build("victim", "3.0", "amd64", "second"), "", "team-b")
	wantKind(t, err, Conflict)

	// Once approved, a foreign package decides its version like any other.
	foreign, err := f.in.Ingest(ctx, bytes.NewReader(debtest.Build("victim", "4.0", "amd64", "squat")), Request{
		Owner: SourceOwner("other"), Origin: "source", OriginRef: "x", VerifyStatus: "unverified",
		HoldOnOwnerConflict: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	err = f.in.Store.Tx(ctx, func(q *store.Queries) error { // as the approve API does
		_, err := q.SetState(ctx, foreign.Package.ID, foreign.Distribution, store.Active)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.upload(t, debtest.Build("victim", "4.0", "amd64", "real"), "", "team-b")
	wantKind(t, err, Conflict)
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

// registryFormat stands in for a format registry sources deliver.
type registryFormat struct{ deb.Format }

func (registryFormat) FromRegistries() {}

func TestReservationsFollowSourceKind(t *testing.T) {
	cfg := config.Default()
	cfg.Sources = []config.Source{
		{Name: "tool", Forge: "gh", Owns: []string{"tool"}},
		{Name: "image", Registry: "r", Image: "a/tool", Owns: []string{"team/*"}},
	}
	tests := []struct {
		fm   format.Format
		name string
		want string
	}{
		{deb.Format{}, "tool", "source:tool"},
		{registryFormat{}, "tool", ""},
		{registryFormat{}, "team/tools/cli", "source:image"},
		{deb.Format{}, "team/x", ""},
	}
	for _, tt := range tests {
		if got := ReservedOwner(cfg, tt.fm, tt.name); got != tt.want {
			t.Errorf("ReservedOwner(%T, %q) = %q, want %q", tt.fm, tt.name, got, tt.want)
		}
	}
}
