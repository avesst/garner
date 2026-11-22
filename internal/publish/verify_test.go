package publish

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/avesst/garner/internal/deb"
	"github.com/avesst/garner/internal/format"
	"github.com/avesst/garner/internal/ingest"
	"github.com/avesst/garner/internal/rpm"
	"github.com/avesst/garner/internal/signing"
)

func (f *fixture) verify(t *testing.T) *VerifyReport {
	t.Helper()
	rep, err := f.pub.Verify(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func hasProblem(rep *VerifyReport, severity, check string) bool {
	for _, p := range rep.Problems {
		if p.Severity == severity && p.Check == check {
			return true
		}
	}
	return false
}

// published gives a fixture with two snapshots, so the second has index
// history to check.
func published(t *testing.T) (*fixture, string) {
	t.Helper()
	f := newFixture(t)
	f.add(t, "app", "1.0", "amd64")
	f.add(t, "libfoo", "2:0.1-1", "all")
	f.publish(t)
	f.add(t, "app", "1.1", "amd64")
	snap := f.publish(t)
	return f, snap.ID
}

func TestVerifyCleanPublish(t *testing.T) {
	f, id := published(t)
	rep := f.verify(t)
	if len(rep.Problems) != 0 {
		t.Fatalf("problems on a clean publish: %+v", rep.Problems)
	}
	if rep.Snapshot != id || rep.Packages != 3 || rep.PublishPending {
		t.Errorf("report %+v", rep)
	}
}

func TestVerifyFindsBrokenState(t *testing.T) {
	pool := func(f *fixture, t *testing.T) string {
		t.Helper()
		var found string
		filepath.WalkDir(filepath.Join(f.root, "pool", "deb"), func(p string, d os.DirEntry, err error) error {
			if err == nil && strings.HasSuffix(p, "app_1.1_amd64.deb") {
				found = p
			}
			return nil
		})
		if found == "" {
			t.Fatal("pool file not found")
		}
		return found
	}
	suite := func(f *fixture, id string) string {
		return filepath.Join(f.root, "snapshots", id, "deb", "dists", "stable")
	}
	tests := []struct {
		name     string
		breakIt  func(t *testing.T, f *fixture, id string)
		severity string
		check    string
	}{
		{"pool file missing", func(t *testing.T, f *fixture, id string) {
			os.Remove(pool(f, t))
		}, format.SeverityError, "pool"},
		{"pool file truncated", func(t *testing.T, f *fixture, id string) {
			p := pool(f, t)
			os.Remove(p) // a hardlink; truncating it would also change the blob
			os.WriteFile(p, []byte("short"), 0o644)
		}, format.SeverityError, "pool"},
		{"blob missing", func(t *testing.T, f *fixture, id string) {
			// The pool keeps its own link to the content; only the blob goes.
			var blob string
			filepath.WalkDir(filepath.Join(f.root, "blobs", "sha256"), func(p string, d os.DirEntry, err error) error {
				if err == nil && d.Type().IsRegular() && blob == "" && sameFile(p, pool(f, t)) {
					blob = p
				}
				return nil
			})
			os.Remove(blob)
		}, format.SeverityError, "blob"},
		{"Packages changed", func(t *testing.T, f *fixture, id string) {
			p := filepath.Join(suite(f, id), "main", "binary-amd64", "Packages")
			data, _ := os.ReadFile(p)
			os.Remove(p) // by-hash shares the inode
			os.WriteFile(p, bytes.Replace(data, []byte("Package: app"), []byte("Package: zpp"), 1), 0o644)
		}, format.SeverityError, "release-hash"},
		{"by-hash file missing", func(t *testing.T, f *fixture, id string) {
			dir := filepath.Join(suite(f, id), "main", "binary-amd64", "by-hash", "SHA256")
			entries, _ := os.ReadDir(dir)
			for _, e := range entries {
				os.Remove(filepath.Join(dir, e.Name()))
			}
		}, format.SeverityError, "by-hash"},
		{"InRelease unsigned", func(t *testing.T, f *fixture, id string) {
			data, _ := os.ReadFile(filepath.Join(suite(f, id), "Release"))
			os.WriteFile(filepath.Join(suite(f, id), "InRelease"), data, 0o644)
		}, format.SeverityError, "signature"},
		{"Release.gpg missing", func(t *testing.T, f *fixture, id string) {
			os.Remove(filepath.Join(suite(f, id), "Release.gpg"))
		}, format.SeverityError, "signature"},
		{"index history missing", func(t *testing.T, f *fixture, id string) {
			// Keep only the files the current Release lists.
			cur, _ := os.ReadFile(filepath.Join(suite(f, id), "Release"))
			dir := filepath.Join(suite(f, id), "main", "binary-amd64", "by-hash", "SHA256")
			entries, _ := os.ReadDir(dir)
			for _, e := range entries {
				if !strings.Contains(string(cur), e.Name()) {
					os.Remove(filepath.Join(dir, e.Name()))
				}
			}
		}, format.SeverityError, "index-history"},
		{"Packages lists a package the snapshot does not serve", func(t *testing.T, f *fixture, id string) {
			if _, err := f.db.Exec(`DELETE FROM snapshot_packages WHERE snapshot_id = ? AND package_id =
				(SELECT id FROM packages WHERE name = 'libfoo')`, id); err != nil {
				t.Fatal(err)
			}
		}, format.SeverityError, "packages"},
		{"Packages misses a package the snapshot serves", func(t *testing.T, f *fixture, id string) {
			f.add(t, "missing", "1.0", "amd64")
			if _, err := f.db.Exec(`INSERT INTO snapshot_packages (snapshot_id, package_id, distribution, served_sha256)
				SELECT ?, id, 'stable', blob_sha256 FROM packages WHERE name = 'missing'`, id); err != nil {
				t.Fatal(err)
			}
		}, format.SeverityError, "packages"},
		{"current points elsewhere", func(t *testing.T, f *fixture, id string) {
			snaps, _ := f.pub.Store.Snapshots(context.Background())
			cur := filepath.Join(f.root, "public", "current")
			os.Remove(cur)
			os.Symlink("../snapshots/"+snaps[1].ID, cur)
		}, format.SeverityError, "current"},
		{"fixed link missing", func(t *testing.T, f *fixture, id string) {
			os.Remove(filepath.Join(f.root, "public", "deb", "dists"))
		}, format.SeverityError, "link"},
		{"snapshot directory missing", func(t *testing.T, f *fixture, id string) {
			snaps, _ := f.pub.Store.Snapshots(context.Background())
			os.RemoveAll(filepath.Join(f.root, "snapshots", snaps[1].ID))
		}, format.SeverityError, "snapshot"},
		{"published key replaced", func(t *testing.T, f *fixture, id string) {
			other := filepath.Join(t.TempDir(), "other.asc")
			signing.GenerateBits(other, "other", "o@example.com", 2048)
			k, _ := signing.Load([]signing.KeySource{{KeyFile: other}})
			armored, binary, _ := k.PublicKeys()
			os.WriteFile(filepath.Join(f.root, "snapshots", id, "key.asc"), armored, 0o644)
			os.WriteFile(filepath.Join(f.root, "snapshots", id, "key.gpg"), binary, 0o644)
		}, format.SeverityError, "keys"},
		{"unpublished upload", func(t *testing.T, f *fixture, id string) {
			f.add(t, "late", "1.0", "amd64")
		}, format.SeverityWarning, "publish-pending"},
		{"stray pool file", func(t *testing.T, f *fixture, id string) {
			p := filepath.Join(f.root, "pool", "deb", "main", "s", "stray", "0123456789abcdef", "stray_1_amd64.deb")
			os.MkdirAll(filepath.Dir(p), 0o755)
			os.WriteFile(p, []byte("x"), 0o644)
		}, format.SeverityWarning, "garbage"},
		{"old temp file", func(t *testing.T, f *fixture, id string) {
			p := filepath.Join(f.root, "tmp", "upload-123")
			os.MkdirAll(filepath.Dir(p), 0o755)
			os.WriteFile(p, []byte("x"), 0o644)
			old := time.Now().Add(-2 * f.cfg.Storage.GCGrace)
			os.Chtimes(p, old, old)
		}, format.SeverityWarning, "garbage"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, id := published(t)
			tt.breakIt(t, f, id)
			rep := f.verify(t)
			if !hasProblem(rep, tt.severity, tt.check) {
				t.Errorf("no %s %q problem; got %+v", tt.severity, tt.check, rep.Problems)
			}
			if tt.severity == format.SeverityWarning && rep.Errors > 0 {
				t.Errorf("errors on top of the expected warning: %+v", rep.Problems)
			}
		})
	}
}

func sameFile(a, b string) bool {
	fa, err1 := os.Stat(a)
	fb, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(fa, fb)
}

func TestVerifyWithoutSnapshot(t *testing.T) {
	f := newFixture(t)
	rep := f.verify(t)
	if rep.Errors != 1 || !hasProblem(rep, format.SeverityError, "snapshot") {
		t.Errorf("report %+v", rep)
	}
}

// rpmFixture publishes deb and rpm, with the rpm tools; it skips without
// them.
func rpmFixture(t *testing.T) *fixture {
	t.Helper()
	for _, tool := range []string{"createrepo_c", "rpmsign", "gpg", "gpgconf"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	f := newFixture(t)
	rf, err := rpm.NewFormat(func() signing.Signer { return f.keys })
	if err != nil {
		t.Fatal(err)
	}
	reg, _ := format.NewRegistry(deb.Format{}, rf)
	f.in.Formats, f.pub.Formats = reg, reg
	f.cfg.RPM.Architectures = []string{"x86_64", "i686", "aarch64"}
	addRPM := func(name string) {
		data, err := os.ReadFile(filepath.Join("..", "rpm", "testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.in.Ingest(context.Background(), bytes.NewReader(data),
			ingest.Request{Owner: "team", Origin: "upload", OriginRef: "t", VerifyStatus: "n/a"}); err != nil {
			t.Fatal(err)
		}
	}
	addRPM("garner-hello-1.0-1.noarch.rpm")
	addRPM("garner-hello-2.0-1.x86_64.rpm")
	addRPM("garner-hello-2.0-1.i686.rpm")
	f.add(t, "app", "1.0", "amd64")
	f.publish(t)
	// A second snapshot with other repodata, so it carries history.
	addRPM("garner-renamed-1.0-1.noarch.rpm")
	f.publish(t)
	return f
}

func TestVerifyRPM(t *testing.T) {
	repodata := func(f *fixture, id string) string {
		return filepath.Join(f.root, "snapshots", id, "rpm", "stable", "x86_64", "repodata")
	}
	tests := []struct {
		name    string
		breakIt func(t *testing.T, f *fixture, id string)
		check   string
	}{
		{"clean", nil, ""},
		{"repomd.xml.asc missing", func(t *testing.T, f *fixture, id string) {
			os.Remove(filepath.Join(repodata(f, id), "repomd.xml.asc"))
		}, "signature"},
		{"primary changed", func(t *testing.T, f *fixture, id string) {
			matches, _ := filepath.Glob(filepath.Join(repodata(f, id), "*-primary.xml.zst"))
			for _, m := range matches {
				os.Remove(m)
				os.WriteFile(m, []byte("not zstd"), 0o644)
			}
		}, "repodata"},
		{"pool file missing", func(t *testing.T, f *fixture, id string) {
			filepath.WalkDir(filepath.Join(f.root, "pool", "rpm"), func(p string, d os.DirEntry, err error) error {
				if err == nil && strings.HasSuffix(p, ".x86_64.rpm") {
					os.Remove(p)
				}
				return nil
			})
		}, "pool"},
		{"primary lists a package the snapshot does not serve", func(t *testing.T, f *fixture, id string) {
			if _, err := f.db.Exec(`DELETE FROM snapshot_packages WHERE snapshot_id = ? AND package_id IN
				(SELECT id FROM packages WHERE format = 'rpm' AND arch = 'noarch')`, id); err != nil {
				t.Fatal(err)
			}
		}, "primary"},
		{"repodata history missing", func(t *testing.T, f *fixture, id string) {
			md, _ := os.ReadFile(filepath.Join(repodata(f, id), "repomd.xml"))
			entries, _ := os.ReadDir(repodata(f, id))
			for _, e := range entries {
				if !strings.HasPrefix(e.Name(), "repomd") && !bytes.Contains(md, []byte(e.Name())) {
					os.Remove(filepath.Join(repodata(f, id), e.Name()))
				}
			}
		}, "index-history"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := rpmFixture(t)
			cur, _ := f.pub.Store.CurrentSnapshot(context.Background())
			if tt.breakIt == nil {
				if rep := f.verify(t); len(rep.Problems) != 0 {
					t.Fatalf("problems on a clean publish: %+v", rep.Problems)
				}
				return
			}
			tt.breakIt(t, f, cur.ID)
			rep := f.verify(t)
			if !hasProblem(rep, format.SeverityError, tt.check) {
				t.Errorf("no error %q problem; got %+v", tt.check, rep.Problems)
			}
		})
	}
}

// A format without packages still gets its pool directory, so the pool
// links of public/ and the snapshot resolve.
func TestVerifyPublishWithoutPackages(t *testing.T) {
	f := newFixture(t)
	f.publish(t)
	if rep := f.verify(t); len(rep.Problems) != 0 {
		t.Fatalf("problems: %+v", rep.Problems)
	}
}

// i686 packages are multilib: listed in the x86_64 repository, with no
// repository of their own, which no host would read.
func TestRPMMultilib(t *testing.T) {
	f := rpmFixture(t)
	cur, _ := f.pub.Store.CurrentSnapshot(context.Background())
	rel := filepath.Join(f.root, "snapshots", cur.ID, "rpm", "stable")
	// The current primary, not one carried over as history.
	md, _ := os.ReadFile(filepath.Join(rel, "x86_64", "repodata", "repomd.xml"))
	m := regexp.MustCompile(`href="(repodata/[^"]*-primary\.xml\.zst)"`).FindSubmatch(md)
	if m == nil {
		t.Fatalf("no primary in repomd.xml:\n%s", md)
	}
	out, err := exec.Command("zstd", "-dc", filepath.Join(rel, "x86_64", string(m[1]))).Output()
	if err != nil {
		t.Skipf("zstd: %v", err)
	}
	for _, want := range []string{"<arch>x86_64</arch>", "<arch>i686</arch>", "<arch>noarch</arch>"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("x86_64 primary lacks %s", want)
		}
	}
	for _, dir := range []string{filepath.Join(rel, "i686"), filepath.Join(f.root, "public", "rpm", "stable", "i686")} {
		if _, err := os.Lstat(dir); err == nil {
			t.Errorf("%s exists; i686 has no repository of its own", dir)
		}
	}
	if _, err := os.Stat(filepath.Join(rel, "aarch64", "repodata", "repomd.xml")); err != nil {
		t.Errorf("aarch64 keeps its repository: %v", err)
	}
	if rep := f.verify(t); len(rep.Problems) != 0 {
		t.Errorf("problems: %+v", rep.Problems)
	}
}
