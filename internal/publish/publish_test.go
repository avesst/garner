package publish

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avesst/garner/internal/blob"
	"github.com/avesst/garner/internal/config"
	"github.com/avesst/garner/internal/db"
	"github.com/avesst/garner/internal/deb"
	"github.com/avesst/garner/internal/deb/debtest"
	"github.com/avesst/garner/internal/format"
	"github.com/avesst/garner/internal/ingest"
	"github.com/avesst/garner/internal/signing"
	"github.com/avesst/garner/internal/store"
)

type fixture struct {
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
	f := &fixture{root: root, cfg: cfg, keys: keys}
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
	if page := f.read(t, "index.html"); !strings.Contains(page, f.keys.Fingerprint()) || !strings.Contains(page, snap.ID) {
		t.Error("landing page lacks fingerprint or snapshot id")
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
