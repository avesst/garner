package publish

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/avesst/garner/internal/blob"
	"github.com/avesst/garner/internal/config"
	"github.com/avesst/garner/internal/db"
	"github.com/avesst/garner/internal/deb"
	"github.com/avesst/garner/internal/deb/debtest"
	"github.com/avesst/garner/internal/format"
	"github.com/avesst/garner/internal/id"
	"github.com/avesst/garner/internal/ingest"
	"github.com/avesst/garner/internal/signing"
	"github.com/avesst/garner/internal/store"
)

type fixture struct {
	db   *sql.DB
	root string
	cfg  *config.Config
	in   *ingest.Ingester
	pub  *Publisher
	keys *signing.Keyring
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	d, err := db.Open(context.Background(), filepath.Join(root, "garner.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	blobs, err := blob.Open(filepath.Join(root, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(root, "key.asc")
	if _, err := signing.GenerateBits(keyFile, "test", "test@example.com", 2048); err != nil {
		t.Fatal(err)
	}
	keys, err := signing.Load([]signing.KeySource{{KeyFile: keyFile}})
	if err != nil {
		t.Fatal(err)
	}
	reg, _ := format.NewRegistry(deb.Format{})
	cfg := config.Default()
	cfg.Server.PublicURL = "http://repo.test"
	cfg.Deb.Architectures = []string{"amd64", "arm64"}
	cfg.Defaults.Retention = 2
	cfg.Hash = "cafe"
	f := &fixture{db: d.DB, root: root, cfg: cfg, keys: keys}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st := store.New(d.DB)
	f.in = &ingest.Ingester{Store: st, Blobs: blobs, Formats: reg, Config: func() *config.Config { return f.cfg },
		TmpDir: root, Log: log}
	f.pub = &Publisher{Store: st, Blobs: blobs, Formats: reg, Config: func() *config.Config { return f.cfg },
		Signer: func() signing.Signer { return f.keys }, Root: root, Log: log}
	return f
}

func (f *fixture) add(t *testing.T, name, version, arch string) {
	t.Helper()
	_, err := f.in.Ingest(context.Background(), bytes.NewReader(debtest.Build(name, version, arch, name+version)),
		ingest.Request{Owner: "team", Origin: "upload", OriginRef: "t", VerifyStatus: "n/a"})
	if err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) publish(t *testing.T) *store.Snapshot {
	t.Helper()
	s, err := f.pub.Publish(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (f *fixture) read(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.root, "public", rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPublishDeb(t *testing.T) {
	f := newFixture(t)
	f.add(t, "app", "1.0", "amd64")
	f.add(t, "libfoo", "2:0.1-1", "all")
	snap := f.publish(t)

	if target, _ := os.Readlink(filepath.Join(f.root, "public", "current")); target != "../snapshots/"+snap.ID {
		t.Errorf("current -> %q", target)
	}
	pk := f.read(t, "deb/dists/stable/main/binary-amd64/Packages")
	for _, want := range []string{
		"Package: app\n", "Filename: pool/main/a/app/", "/app_1.0_amd64.deb\n",
		"Package: libfoo\n", "Version: 2:0.1-1\n", "Filename: pool/main/libf/libfoo/", "/libfoo_0.1-1_all.deb\n",
	} {
		if !strings.Contains(pk, want) {
			t.Errorf("amd64 Packages lacks %q:\n%s", want, pk)
		}
	}
	arm := f.read(t, "deb/dists/stable/main/binary-arm64/Packages")
	if strings.Contains(arm, "Package: app\n") || !strings.Contains(arm, "Package: libfoo\n") {
		t.Errorf("arm64 Packages should list only the all package:\n%s", arm)
	}

	// Every file Release lists exists with that hash, also by hash.
	release := f.read(t, "deb/dists/stable/Release")
	if !strings.Contains(release, "Architectures: amd64 arm64\n") || !strings.Contains(release, "Acquire-By-Hash: yes\n") {
		t.Errorf("Release header:\n%s", release)
	}
	_, sha := splitRelease(release)
	if len(sha) != 6 {
		t.Fatalf("Release lists %d SHA256 files, want 6", len(sha))
	}
	for path, sum := range sha {
		data := f.read(t, "deb/dists/stable/"+path)
		got := sha256.Sum256([]byte(data))
		if hex.EncodeToString(got[:]) != sum {
			t.Errorf("%s: sha256 mismatch", path)
		}
		f.read(t, "deb/dists/stable/"+filepath.Dir(path)+"/by-hash/SHA256/"+sum)
	}

	// The pool file is reachable through the public links.
	for _, line := range strings.Split(pk, "\n") {
		if name, ok := strings.CutPrefix(line, "Filename: "); ok {
			f.read(t, "deb/"+name)
		}
	}
	// Pinned snapshots resolve the pool too.
	if _, err := os.Stat(filepath.Join(f.root, "public/snapshots", snap.ID, "deb/pool/main/a/app")); err != nil {
		t.Errorf("snapshot pool link: %v", err)
	}
	for _, rootFile := range []string{"index.html", "key.asc", "key.gpg", "garner.sources"} {
		f.read(t, rootFile)
	}
	if src := f.read(t, "garner.sources"); !strings.Contains(src, "URIs: http://repo.test/deb\n") {
		t.Errorf("garner.sources:\n%s", src)
	}
	page := f.read(t, "index.html")
	fp := f.keys.Fingerprint()
	if !strings.Contains(page, "<span>"+fp[:4]+"</span> <span>"+fp[4:8]+"</span>") || !strings.Contains(page, snap.ID) {
		t.Error("landing page lacks fingerprint or snapshot id")
	}
	// Packages: one amd64 and one arch-independent in stable; arm64 is
	// configured but empty.
	for _, want := range []string{
		`<th scope="col">amd64</th><th scope="col">arm64</th><th scope="col">all</th>`,
		`<td class="empty">0</td>`,
		"2 packages in this snapshot.",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("landing page lacks %q", want)
		}
	}
	if strings.Contains(page, "ZgotmplZ") {
		t.Error("landing page has a value the template escaper rejected")
	}
	// The same page is served inside snapshots/<id>/, so the status URL is
	// resolved from the page's location. The snapshot ID is shown, not linked.
	if !strings.Contains(page, `fetch(base + "api/v1/status"`) ||
		strings.Contains(page, `href="/snapshots/`) || strings.Contains(page, `href="snapshots/`) {
		t.Error("landing page does not resolve the status URL from its location or links to the snapshot")
	}

	if gpgv, err := exec.LookPath("gpgv"); err == nil {
		_, bin, _ := f.keys.PublicKeys()
		kr := filepath.Join(f.root, "kr.gpg")
		os.WriteFile(kr, bin, 0o644)
		dists := filepath.Join(f.root, "public/deb/dists/stable")
		for _, args := range [][]string{
			{filepath.Join(dists, "InRelease")},
			{filepath.Join(dists, "Release.gpg"), filepath.Join(dists, "Release")},
		} {
			if out, err := exec.Command(gpgv, append([]string{"--keyring", kr}, args...)...).CombinedOutput(); err != nil {
				t.Errorf("gpgv %v: %v\n%s", args, err, out)
			}
		}
	}
}

// splitRelease returns the MD5Sum and SHA256 entries of a Release file.
func splitRelease(release string) (md5, sha map[string]string) {
	md5, sha = map[string]string{}, map[string]string{}
	var cur map[string]string
	for _, line := range strings.Split(release, "\n") {
		switch {
		case line == "MD5Sum:":
			cur = md5
		case line == "SHA256:":
			cur = sha
		case strings.HasPrefix(line, " ") && cur != nil:
			f := strings.Fields(line)
			cur[f[2]] = f[0]
		default:
			cur = nil
		}
	}
	return md5, sha
}

func TestIndexHistoryIsCarried(t *testing.T) {
	f := newFixture(t)
	f.add(t, "app", "1.0", "amd64")
	f.publish(t)
	_, oldSHA := splitRelease(f.read(t, "deb/dists/stable/Release"))
	f.add(t, "app", "1.1", "amd64")
	f.publish(t)
	_, newSHA := splitRelease(f.read(t, "deb/dists/stable/Release"))

	p := "main/binary-amd64/Packages"
	if oldSHA[p] == newSHA[p] {
		t.Fatal("Packages did not change between publishes")
	}
	// A client that read the old InRelease can still fetch its Packages.
	f.read(t, "deb/dists/stable/main/binary-amd64/by-hash/SHA256/"+oldSHA[p])
	if strings.Contains(f.read(t, "deb/dists/stable/Release"), oldSHA[p]) {
		t.Error("the new Release lists the old Packages")
	}

	// Outside the history window, only the previous snapshot is carried.
	f.cfg.Storage.IndexHistory = 0
	f.add(t, "app", "1.2", "amd64")
	f.publish(t)
	f.add(t, "app", "1.3", "amd64")
	f.publish(t)
	if _, err := os.Stat(filepath.Join(f.root, "public/deb/dists/stable/main/binary-amd64/by-hash/SHA256", oldSHA[p])); err == nil {
		t.Error("index from three publishes ago is still carried with index_history 0")
	}
}

func TestRetentionAndRollback(t *testing.T) {
	f := newFixture(t)
	f.add(t, "app", "1.0", "amd64")
	first := f.publish(t)
	f.add(t, "app", "1.1", "amd64")
	f.add(t, "app", "1.10", "amd64")
	f.publish(t)
	pk := f.read(t, "deb/dists/stable/main/binary-amd64/Packages")
	if strings.Contains(pk, "Version: 1.0\n") || !strings.Contains(pk, "Version: 1.10\n") || !strings.Contains(pk, "Version: 1.1\n") {
		t.Errorf("retention 2 should keep 1.10 and 1.1:\n%s", pk)
	}

	rb, err := f.pub.Rollback(context.Background(), first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rb.ID == first.ID {
		t.Error("rollback reused the old snapshot")
	}
	pk = f.read(t, "deb/dists/stable/main/binary-amd64/Packages")
	if !strings.Contains(pk, "Version: 1.0\n") || strings.Contains(pk, "Version: 1.1") {
		t.Errorf("after rollback:\n%s", pk)
	}
	// The next publish keeps the rolled-back state.
	f.publish(t)
	if pk2 := f.read(t, "deb/dists/stable/main/binary-amd64/Packages"); pk2 != pk {
		t.Errorf("publish after rollback changed Packages:\n%s", pk2)
	}
}

// Packages of a format that is not enabled, for example rpm while its
// tools are missing, are neither published nor dropped.
func TestRetainLeavesDisabledFormatAlone(t *testing.T) {
	f := newFixture(t)
	pkgs := []*format.Package{
		{Format: "deb", Name: "app", Version: "1.0", Distribution: "stable", Arch: "amd64"},
		{Format: "rpm", Name: "app", Version: "1.0", Release: "1", Distribution: "stable", Arch: "x86_64"},
	}
	keep, drop := f.pub.retain(f.cfg, pkgs, nil)
	if len(keep) != 1 || keep[0].Format != "deb" {
		t.Errorf("keep = %v, want only the deb", keep)
	}
	if len(drop) != 0 {
		t.Errorf("drop = %v, want none", drop)
	}
}

func TestStaleComparesInputs(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	stale := func() bool {
		t.Helper()
		s, err := f.pub.Stale(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	if !stale() {
		t.Error("without a snapshot: want stale")
	}
	f.add(t, "app", "1.0", "amd64")
	f.publish(t)
	if stale() {
		t.Error("right after a publish: want up to date")
	}

	otherKey := filepath.Join(f.root, "other.asc")
	if _, err := signing.GenerateBits(otherKey, "other", "other@example.com", 2048); err != nil {
		t.Fatal(err)
	}
	changes := []struct {
		name   string
		change func()
	}{
		{"package", func() { f.add(t, "app", "1.1", "amd64") }},
		{"version", func() { f.pub.Version = "2.0" }},
		{"build", func() { f.pub.Build = "other" }},
		{"config", func() { f.cfg.Hash = "beef" }},
		{"config commit", func() { f.pub.ConfigCommit = func() string { return "abc123" } }},
		{"signing key", func() {
			k, err := signing.Load([]signing.KeySource{{KeyFile: otherKey}})
			if err != nil {
				t.Fatal(err)
			}
			f.keys = k
		}},
	}
	for _, c := range changes {
		c.change()
		if !stale() {
			t.Errorf("after a %s change: want stale", c.name)
		}
		f.publish(t)
		if stale() {
			t.Errorf("after publishing the %s change: want up to date", c.name)
		}
	}
}

func TestGC(t *testing.T) {
	f := newFixture(t)
	f.cfg.Storage.SnapshotRetention = 2
	f.cfg.Storage.GCGrace = 0
	f.cfg.Defaults.Retention = 1
	f.add(t, "app", "1.0", "amd64")
	s1 := f.publish(t)
	f.add(t, "app", "2.0", "amd64")
	f.publish(t)
	f.add(t, "app", "3.0", "amd64")
	f.publish(t)

	res, err := f.pub.GC(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Snapshots) != 1 || res.Snapshots[0] != s1.ID {
		t.Errorf("collected snapshots %v, want [%s]", res.Snapshots, s1.ID)
	}
	// 1.0 was only in s1: its blob and pool entry go. 2.0 is in the
	// retained previous snapshot and stays.
	if res.Blobs != 1 || res.PoolFiles != 1 {
		t.Errorf("GC = %+v, want 1 blob and 1 pool file", res)
	}
	pkgs, _ := f.in.Store.Placed(context.Background(), store.PackageFilter{})
	if len(pkgs) != 2 {
		t.Errorf("%d packages left, want 2", len(pkgs))
	}

	// With a grace period, a just-replaced snapshot is kept.
	f.cfg.Storage.GCGrace = time.Hour
	f.add(t, "app", "4.0", "amd64")
	f.publish(t)
	res, err = f.pub.GC(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Snapshots) != 0 {
		t.Errorf("collected %v within gc_grace", res.Snapshots)
	}
}

// resign gives the package name a new signed variant with the given
// content, as Ingester.Resign does after a key change, and returns its
// sha256.
func (f *fixture) resign(t *testing.T, name, content string) string {
	t.Helper()
	ctx := context.Background()
	b, err := f.pub.Blobs.Put(strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	err = f.pub.Store.Tx(ctx, func(q *store.Queries) error {
		pkgs, err := q.Placed(ctx, store.PackageFilter{Name: name})
		if err != nil {
			return err
		}
		if err := q.AddBlob(ctx, b.SHA256, b.Size); err != nil {
			return err
		}
		return q.SetSignedBlob(ctx, pkgs[0].ID, b.SHA256)
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.SHA256
}

// servedFiles returns the pool files a snapshot's amd64 Packages index
// lists, as paths below public/.
func (f *fixture) servedFiles(t *testing.T, snapshotDir string) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(f.read(t, filepath.Join(snapshotDir, "deb/dists/stable/main/binary-amd64/Packages")), "\n") {
		if name, ok := strings.CutPrefix(line, "Filename: "); ok {
			out = append(out, filepath.Join("deb", name))
		}
	}
	return out
}

// A re-sign replaces the blob a package is served from. Until a snapshot
// that lists the new blob replaces the current one, GC must keep the old.
func TestGCKeepsFilesOfCurrentSnapshotAfterResign(t *testing.T) {
	f := newFixture(t)
	f.cfg.Storage.GCGrace = 0
	f.add(t, "app", "1.0", "amd64")
	f.resign(t, "app", "signed with the old key")
	f.publish(t)
	served := f.servedFiles(t, "current")
	if len(served) != 1 {
		t.Fatalf("current snapshot lists %v, want one file", served)
	}

	f.resign(t, "app", "signed with the new key")
	if _, err := f.pub.GC(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, rel := range served {
		if _, err := os.Stat(filepath.Join(f.root, "public", rel)); err != nil {
			t.Errorf("current snapshot serves %s, which GC removed: %v", rel, err)
		}
	}
}

// After a re-sign and a publish, the previous snapshot is retained and
// keeps serving the old signed blob; once GC collects that snapshot, the
// blob goes too.
func TestGCKeepsResignedBlobWhileSnapshotRetained(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.cfg.Storage.SnapshotRetention = 2
	f.cfg.Storage.GCGrace = 0
	f.add(t, "app", "1.0", "amd64")
	old := f.resign(t, "app", "signed with the old key")
	f.publish(t)
	served := f.servedFiles(t, "current")

	f.resign(t, "app", "signed with the new key")
	f.publish(t)
	if _, err := f.pub.GC(ctx); err != nil {
		t.Fatal(err)
	}
	for _, rel := range served {
		if _, err := os.Stat(filepath.Join(f.root, "public", rel)); err != nil {
			t.Errorf("retained snapshot serves %s, which GC removed: %v", rel, err)
		}
	}

	f.add(t, "other", "1.0", "amd64")
	f.publish(t)
	res, err := f.pub.GC(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Snapshots) != 1 {
		t.Fatalf("collected snapshots %v, want the first", res.Snapshots)
	}
	path, err := f.pub.Blobs.Path(old)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("old signed blob kept after its last snapshot was collected: %v", err)
	}
	for _, rel := range served {
		if _, err := os.Stat(filepath.Join(f.root, "public", rel)); !os.IsNotExist(err) {
			t.Errorf("pool file %s kept after its last snapshot was collected: %v", rel, err)
		}
	}
}

// failPublish runs a publish that fails at the swap of public/current,
// after its snapshot was rendered and recorded.
func (f *fixture) failPublish(t *testing.T) {
	t.Helper()
	pub := filepath.Join(f.root, "public")
	if err := os.Chmod(pub, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(pub, 0o755)
	if _, err := f.pub.Publish(context.Background()); err == nil {
		t.Fatal("publish succeeded, want it to fail at the swap")
	}
}

// snapshotIDs returns the recorded snapshots, newest first, and the
// snapshot directories on disk.
func (f *fixture) snapshotIDs(t *testing.T) (rows, dirs []string) {
	t.Helper()
	snaps, err := f.pub.Store.Snapshots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range snaps {
		rows = append(rows, s.ID)
	}
	entries, err := os.ReadDir(filepath.Join(f.root, "snapshots"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		dirs = append(dirs, e.Name())
	}
	return rows, dirs
}

// current returns the snapshot public/current points to.
func (f *fixture) current(t *testing.T) string {
	t.Helper()
	target, err := os.Readlink(filepath.Join(f.root, "public", "current"))
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Base(target)
}

// A publish that fails before public/current switches leaves nothing
// behind, so retries cannot push served snapshots out of retention.
func TestFailedPublishLeavesNoSnapshot(t *testing.T) {
	f := newFixture(t)
	f.cfg.Storage.SnapshotRetention = 3
	f.cfg.Storage.GCGrace = 0
	f.add(t, "app", "1.0", "amd64")
	s1 := f.publish(t)
	f.add(t, "app", "2.0", "amd64")
	s2 := f.publish(t)
	for range 3 {
		f.failPublish(t) // as the scheduler retries
	}
	rows, dirs := f.snapshotIDs(t)
	want := []string{s2.ID, s1.ID}
	if !slices.Equal(rows, want) {
		t.Errorf("snapshot rows %v, want %v", rows, want)
	}
	if !slices.Equal(dirs, []string{s1.ID, s2.ID}) { // ULIDs sort by time
		t.Errorf("snapshot directories %v, want %v", dirs, []string{s1.ID, s2.ID})
	}
	res, err := f.pub.GC(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Snapshots) != 0 {
		t.Errorf("GC collected %v, want both served snapshots kept", res.Snapshots)
	}
	if cur := f.current(t); cur != s2.ID {
		t.Errorf("current = %s, want %s", cur, s2.ID)
	}
}

// A publish can fail after the swap (the directory sync or SetCurrent).
// public/current then serves the snapshot, so discard must keep it.
func TestDiscardKeepsLinkedSnapshot(t *testing.T) {
	f := newFixture(t)
	f.add(t, "app", "1.0", "amd64")
	s := f.publish(t)
	f.pub.discard(context.Background(), s.ID)
	rows, dirs := f.snapshotIDs(t)
	if !slices.Equal(rows, []string{s.ID}) || !slices.Equal(dirs, []string{s.ID}) {
		t.Errorf("rows %v, directories %v; want the linked snapshot %s kept", rows, dirs, s.ID)
	}
}

// cancelOnSign cancels the publish context while the snapshot renders,
// as a shutdown arriving mid-publish does.
type cancelOnSign struct {
	signing.Signer
	cancel context.CancelFunc
}

func (c cancelOnSign) ClearSign(w io.Writer, data []byte) error {
	c.cancel()
	return c.Signer.ClearSign(w, data)
}

// A publish cancelled after its snapshot was rendered still removes it:
// no row would ever let GC find the directory.
func TestCancelledPublishLeavesNoSnapshot(t *testing.T) {
	f := newFixture(t)
	f.add(t, "app", "1.0", "amd64")
	s := f.publish(t)
	f.add(t, "app", "2.0", "amd64")
	ctx, cancel := context.WithCancel(context.Background())
	keys := f.keys
	f.pub.Signer = func() signing.Signer { return cancelOnSign{keys, cancel} }
	if _, err := f.pub.Publish(ctx); err == nil {
		t.Fatal("publish succeeded, want it cancelled")
	}
	rows, dirs := f.snapshotIDs(t)
	if !slices.Equal(rows, []string{s.ID}) || !slices.Equal(dirs, []string{s.ID}) {
		t.Errorf("rows %v, directories %v; want only %s", rows, dirs, s.ID)
	}
}

// A crash between renaming a snapshot into place and recording it leaves
// a directory without a row. GC removes it once gc_grace has passed.
func TestGCRemovesUnrecordedSnapshot(t *testing.T) {
	f := newFixture(t)
	f.cfg.Storage.GCGrace = time.Hour
	f.add(t, "app", "1.0", "amd64")
	s := f.publish(t)
	orphan := func(age time.Duration) string {
		name := id.New()
		dir := filepath.Join(f.root, "snapshots", name)
		if err := os.MkdirAll(filepath.Join(dir, "deb"), 0o755); err != nil {
			t.Fatal(err)
		}
		at := time.Now().Add(-age)
		if err := os.Chtimes(dir, at, at); err != nil {
			t.Fatal(err)
		}
		return name
	}
	old, fresh := orphan(2*time.Hour), orphan(0)
	res, err := f.pub.GC(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, dirs := f.snapshotIDs(t)
	if want := []string{s.ID, fresh}; !slices.Equal(dirs, want) {
		t.Errorf("directories %v, want %v (old orphan %s removed)", dirs, want, old)
	}
	if !slices.Contains(res.Snapshots, old) {
		t.Errorf("GC reported %v, want %s", res.Snapshots, old)
	}
}

// gc_grace counts from when a snapshot stopped being current. A newer
// snapshot's created_at is set before it renders, so a render longer than
// gc_grace must not cut short the grace of the snapshot it replaces.
func TestGCGraceCountsFromReplacement(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.cfg.Storage.SnapshotRetention = 2
	f.cfg.Storage.GCGrace = time.Hour
	f.add(t, "app", "1.0", "amd64")
	s1 := f.publish(t)
	f.add(t, "app", "2.0", "amd64")
	s2 := f.publish(t)
	f.add(t, "app", "3.0", "amd64")
	f.publish(t)
	// s2 took two hours to render; s1 was current until it finished.
	for id, age := range map[string]time.Duration{s1.ID: 3 * time.Hour, s2.ID: 2 * time.Hour} {
		if _, err := f.db.Exec(`UPDATE snapshots SET created_at = ? WHERE id = ?`,
			store.FormatTime(time.Now().Add(-age)), id); err != nil {
			t.Fatal(err)
		}
	}
	res, err := f.pub.GC(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Snapshots) != 0 {
		t.Errorf("collected %v; s1 %s was replaced moments ago, within gc_grace", res.Snapshots, s1.ID)
	}

	f.cfg.Storage.GCGrace = 0
	res, err = f.pub.GC(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Snapshots, []string{s1.ID}) {
		t.Errorf("collected %v after gc_grace, want [%s]", res.Snapshots, s1.ID)
	}
}

// A snapshot that never became current, left by a publish that crashed
// before the swap, gets gc_grace from its creation.
func TestGCGraceOfSnapshotNeverCurrent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.cfg.Storage.SnapshotRetention = 2
	f.cfg.Storage.GCGrace = time.Hour
	f.publish(t)
	f.publish(t)
	leftover := func(age time.Duration) string {
		s := &store.Snapshot{ID: id.New(), CreatedAt: store.FormatTime(time.Now().Add(-age))}
		if err := f.pub.Store.Tx(ctx, func(q *store.Queries) error { return q.InsertSnapshot(ctx, s, nil, nil) }); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(f.root, "snapshots", s.ID), 0o755); err != nil {
			t.Fatal(err)
		}
		return s.ID
	}
	fresh, stale := leftover(10*time.Minute), leftover(2*time.Hour)
	res, err := f.pub.GC(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Snapshots, []string{stale}) {
		t.Errorf("collected %v, want only the leftover past gc_grace %s (not %s)", res.Snapshots, stale, fresh)
	}
}

func TestLinksAreReconciled(t *testing.T) {
	f := newFixture(t)
	f.publish(t)
	f.cfg.Server.ClientName = "repo"
	f.publish(t)
	if _, err := os.Lstat(filepath.Join(f.root, "public", "garner.sources")); !os.IsNotExist(err) {
		t.Errorf("old client file link still present: %v", err)
	}
	f.read(t, "repo.sources")
}

// The page may be reached through a proxy that strips public_url's path, so
// its path must not be baked into the status URL.
func TestLandingStatusIgnoresPublicURLPath(t *testing.T) {
	f := newFixture(t)
	f.cfg.Server.PublicURL = "http://repo.test/garner/"
	f.publish(t)
	page := f.read(t, "index.html")
	if strings.Contains(page, `garner/api/v1/status`) || strings.Contains(page, `garner\/api\/v1\/status`) {
		t.Error("status URL is built from public_url")
	}
}

// The default suite's client file is served at the root, the others below
// clients/. Each installs as the same local file, and the landing page
// offers all of them.
func TestClientFilePerSuite(t *testing.T) {
	f := newFixture(t)
	f.cfg.Deb.Suites = []string{"testing", "stable"}
	f.publish(t)
	if src := f.read(t, "garner.sources"); !strings.Contains(src, "Suites: stable\n") {
		t.Errorf("garner.sources:\n%s", src)
	}
	src := f.read(t, "clients/testing/garner.sources")
	if !strings.Contains(src, "Suites: testing\n") || !strings.Contains(src, "Signed-By: /usr/share/keyrings/garner.gpg\n") {
		t.Errorf("clients/testing/garner.sources:\n%s", src)
	}
	page := f.read(t, "index.html")
	for _, want := range []string{
		`data-dist="stable"`, `data-dist="testing"`, `<h4>stable (default)</h4>`,
		"/etc/apt/sources.list.d/garner.sources http://repo.test/clients/testing/garner.sources",
		`<a href="clients/testing/garner.sources">garner.sources</a>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("landing page lacks %q", want)
		}
	}
	if strings.Contains(page, "Edit Suites:") || strings.Contains(page, "garner-testing") {
		t.Error("landing page tells users to edit the suite by hand or installs a second source")
	}

	// With one suite left, the clients link goes away.
	f.cfg.Deb.Suites = []string{"stable"}
	f.publish(t)
	if _, err := os.Lstat(filepath.Join(f.root, "public", "clients")); !os.IsNotExist(err) {
		t.Errorf("public/clients after dropping suites: %v", err)
	}
}

// escapingFormat returns a pool path outside the pool, as a format whose
// parser let a hostile field through would.
type escapingFormat struct{ deb.Format }

func (escapingFormat) PoolPath(*format.Package) string { return "../../escape.deb" }

func TestLinkPoolStaysInPool(t *testing.T) {
	f := newFixture(t)
	pkg := &format.Package{Name: "tool", BlobSHA256: strings.Repeat("ab", 32)}
	err := f.pub.linkPool(escapingFormat{}, []*format.Package{pkg})
	if err == nil || !strings.Contains(err.Error(), "leaves the pool") {
		t.Errorf("err = %v, want a pool path error", err)
	}
}

// activeNames returns "name version" of every active placement, sorted.
func (f *fixture) activeNames(t *testing.T) []string {
	t.Helper()
	pkgs, err := f.pub.Store.Placed(context.Background(), store.PackageFilter{States: []string{store.Active}})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, pk := range pkgs {
		out = append(out, pk.Name+" "+pk.Version)
	}
	slices.Sort(out)
	return out
}

// A snapshot published while a format was disabled holds none of that
// format's packages because it did not render the format, not because the
// packages were gone. Rolling back to it must leave them alone, whether
// the format is still disabled or back by then, so GC cannot delete them.
func TestRollbackLeavesFormatsTheSnapshotDidNotRender(t *testing.T) {
	for _, reenable := range []bool{false, true} {
		name := "still disabled"
		if reenable {
			name = "enabled again"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.cfg.Storage.SnapshotRetention = 1
			f.cfg.Storage.GCGrace = 0
			enabled := f.pub.Formats
			none, _ := format.NewRegistry()

			f.add(t, "app", "1.0", "amd64")
			f.publish(t)
			f.pub.Formats = none // its tools went missing
			s2 := f.publish(t)
			if reenable {
				f.pub.Formats = enabled
			}
			if _, err := f.pub.Rollback(context.Background(), s2.ID); err != nil {
				t.Fatal(err)
			}
			if got := f.activeNames(t); !slices.Equal(got, []string{"app 1.0"}) {
				t.Errorf("active after rollback = %v, want [app 1.0]", got)
			}

			f.pub.Formats = enabled
			f.publish(t)
			res, err := f.pub.GC(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if res.Blobs != 0 {
				t.Errorf("GC deleted %d blobs, want none", res.Blobs)
			}
			if got := f.activeNames(t); !slices.Equal(got, []string{"app 1.0"}) {
				t.Errorf("active after GC = %v, want [app 1.0]", got)
			}
			if pk := f.read(t, "deb/dists/stable/main/binary-amd64/Packages"); !strings.Contains(pk, "Package: app\n") {
				t.Errorf("app is not published:\n%s", pk)
			}
		})
	}
}

// A snapshot that rendered a format without any of its packages still
// removes them on rollback: the format was empty then.
func TestRollbackRemovesPackagesOfRenderedEmptyFormat(t *testing.T) {
	f := newFixture(t)
	empty := f.publish(t)
	f.add(t, "app", "1.0", "amd64")
	f.publish(t)
	if _, err := f.pub.Rollback(context.Background(), empty.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.activeNames(t); len(got) != 0 {
		t.Errorf("active after rollback = %v, want none", got)
	}
}
