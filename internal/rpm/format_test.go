package rpm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avesst/garner/internal/config"
	"github.com/avesst/garner/internal/format"
	"github.com/avesst/garner/internal/signing"
)

func TestCarryRepodataOnlyListedFiles(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "old")
	os.MkdirAll(old, 0o755)
	repomd := `<?xml version="1.0"?>
<repomd xmlns="http://linux.duke.edu/metadata/repo">
  <data type="primary"><location href="repodata/aaa-primary.xml.zst"/></data>
  <data type="filelists"><location href="repodata/bbb-filelists.xml.zst"/></data>
</repomd>`
	for name, content := range map[string]string{
		"repomd.xml":            repomd,
		"aaa-primary.xml.zst":   "p",
		"bbb-filelists.xml.zst": "f",
		"ccc-primary.xml.zst":   "carried by the old snapshot itself",
	} {
		os.WriteFile(filepath.Join(old, name), []byte(content), 0o644)
	}
	newDir := filepath.Join(dir, "new")
	os.MkdirAll(newDir, 0o755)
	if err := carryRepodata(old, newDir); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{
		"aaa-primary.xml.zst": true, "bbb-filelists.xml.zst": true,
		"ccc-primary.xml.zst": false, "repomd.xml": false,
	} {
		_, err := os.Stat(filepath.Join(newDir, name))
		if got := err == nil; got != want {
			t.Errorf("%s carried = %v, want %v", name, got, want)
		}
	}
	// A missing earlier snapshot is not an error.
	if err := carryRepodata(filepath.Join(dir, "missing"), newDir); err != nil {
		t.Error(err)
	}
}

// createrepo_c keeps each package's checksum in CacheDir, so a publish
// reads only the headers of unchanged packages, not every file: on NFS a
// full read made each publish fetch the whole rpm repository.
func TestRenderChecksumCache(t *testing.T) {
	createrepo, err := exec.LookPath("createrepo_c")
	if err != nil {
		t.Skip("createrepo_c not installed")
	}
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key.asc")
	if _, err := signing.GenerateBits(keyFile, "garner", "garner@example.com", 2048); err != nil {
		t.Fatal(err)
	}
	k, err := signing.Load([]signing.KeySource{{KeyFile: keyFile}})
	if err != nil {
		t.Fatal(err)
	}
	rpm, err := os.ReadFile(filepath.Join("testdata", "garner-hello-1.0-1.noarch.rpm"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(rpm)
	p := &format.Package{Format: "rpm", Name: "garner-hello", Version: "1.0", Release: "1", Arch: "noarch",
		BlobSHA256: hex.EncodeToString(sum[:]), Distribution: "stable"}
	f := &Format{createrepo: createrepo}
	pool := filepath.Join(dir, "pool")
	poolFile := filepath.Join(pool, filepath.FromSlash(f.PoolPath(p)))
	if err := os.MkdirAll(filepath.Dir(poolFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(poolFile, rpm, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.RPM.Releases = []string{"stable"}
	cfg.RPM.Architectures = []string{"x86_64"}
	cache := filepath.Join(dir, "cache")
	render := func(name, cacheDir string, pkgs []*format.Package) string {
		t.Helper()
		out := filepath.Join(dir, name)
		err := f.Render(context.Background(), &format.RenderRequest{Dir: out, Packages: pkgs,
			Config: cfg, Signer: k, PoolDir: pool, CacheDir: cacheDir})
		if err != nil {
			t.Fatalf("render %s: %v", name, err)
		}
		return out
	}
	entries := func() []string {
		t.Helper()
		es, err := os.ReadDir(filepath.Join(cache, "createrepo-sha256"))
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, e := range es {
			names = append(names, e.Name())
		}
		return names
	}

	setEntry := func(name, value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(cache, "createrepo-sha256", name), []byte(value), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// variant is p served as another blob: a hardlink to the same file at
	// that blob's pool path, so name, header, size and mtime all match.
	variant := func(served, dist string) *format.Package {
		t.Helper()
		q := *p
		q.SignedSHA256, q.Distribution = served, dist
		path := filepath.Join(pool, filepath.FromSlash(f.PoolPath(&q)))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(poolFile, path); err != nil && !os.IsExist(err) {
			t.Fatal(err)
		}
		return &q
	}
	fake := strings.Repeat("ab", 32)

	plain := render("plain", "", []*format.Package{p})
	cached := render("cached", cache, []*format.Package{p})
	if a, b := primaryChecksum(t, plain), primaryChecksum(t, cached); a != b {
		t.Errorf("primary metadata with the cache is %s, without it %s", b, a)
	}
	es := entries()
	if len(es) != 1 || !strings.HasPrefix(es[0], "garner-hello-1.0-1.noarch.rpm-") {
		t.Fatalf("cache holds %v, want one entry for garner-hello", es)
	}

	// An entry whose checksum is not the one garner serves is never used.
	// A re-signed rpm keeps its name and main header, which createrepo_c's
	// key covers, so its old checksum would otherwise be listed.
	setEntry(es[0], fake)
	if got := render("stale", cache, []*format.Package{p}); primaryChecksum(t, got) != primaryChecksum(t, plain) {
		t.Error("a cached checksum other than the served one was used")
	}

	// An entry that matches the served checksum is used, not recomputed:
	// the cached value reaches the metadata although the file differs.
	trusted := variant(fake, "stable")
	setEntry(entries()[0], fake)
	if got := render("trusted", cache, []*format.Package{trusted}); primaryChecksum(t, got) == primaryChecksum(t, plain) {
		t.Error("a cached checksum equal to the served one was not used")
	}

	// One file name served as two blobs, here in two releases, turns the
	// cache off: an entry could only be right for one of them.
	cfg.RPM.Releases = []string{"stable", "other"}
	both := render("both", cache, []*format.Package{p, variant(fake, "other")})
	if primaryChecksum(t, both) != primaryChecksum(t, plain) {
		t.Error("stable lists another checksum when the same file name is served twice")
	}
	if es := entries(); len(es) != 0 {
		t.Errorf("cache holds %v for a file name served as two blobs, want nothing", es)
	}
	cfg.RPM.Releases = []string{"stable"}

	// A package that is no longer rendered loses its entry.
	render("refill", cache, []*format.Package{p})
	render("empty", cache, nil)
	if es := entries(); len(es) != 0 {
		t.Errorf("cache holds %v after rendering without the package, want nothing", es)
	}
}

// primaryChecksum returns the open-checksum of the primary metadata in the
// stable/x86_64 repository below dir.
func primaryChecksum(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "stable", "x86_64", "repodata", "repomd.xml"))
	if err != nil {
		t.Fatal(err)
	}
	var md struct {
		Data []struct {
			Type         string `xml:"type,attr"`
			OpenChecksum string `xml:"open-checksum"`
		} `xml:"data"`
	}
	if err := xml.Unmarshal(data, &md); err != nil {
		t.Fatal(err)
	}
	for _, d := range md.Data {
		if d.Type == "primary" {
			return d.OpenChecksum
		}
	}
	t.Fatalf("no primary metadata in %s", dir)
	return ""
}

func TestPoolPathAndClient(t *testing.T) {
	f := &Format{}
	p := &format.Package{Name: "libfoo", Version: "1.2", Release: "3.el10", Arch: "x86_64",
		BlobSHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}
	if got, want := f.PoolPath(p), "libf/0123456789abcdef/libfoo-1.2-3.el10.x86_64.rpm"; got != want {
		t.Errorf("PoolPath = %s, want %s", got, want)
	}
	cfg := config.Default()
	cfg.RPM.Releases = []string{"el10", "stable"}
	cs := f.Clients(cfg, "https://repo.example.com", nil)
	if len(cs) != 2 {
		t.Fatalf("got %d client files, want 2", len(cs))
	}
	// stable is the default, served at the root. Every release installs as
	// the same file and repo ID, so picking another replaces it.
	for i, want := range []struct{ path, dist, baseurl string }{
		{"garner.repo", "stable", "baseurl=https://repo.example.com/rpm/stable/$basearch/\n"},
		{"clients/el10/garner.repo", "el10", "baseurl=https://repo.example.com/rpm/el10/$basearch/\n"},
	} {
		c := cs[i]
		if c.Name != "garner.repo" || c.Path != want.path || c.Distribution != want.dist ||
			!strings.HasPrefix(string(c.Content), "[garner]\n") || !strings.Contains(string(c.Content), want.baseurl) ||
			!strings.Contains(c.Instructions, "/etc/yum.repos.d/garner.repo https://repo.example.com/"+want.path) {
			t.Errorf("client %d = %s at %s (%s):\n%s\n%s", i, c.Name, c.Path, c.Distribution, c.Content, c.Instructions)
		}
	}
}

// A pool file name that does not fit a path component would make every
// publish fail, so Parse rejects it.
func TestParsePoolFileNameLimit(t *testing.T) {
	for _, tc := range []struct {
		version string
		ok      bool
	}{
		// foo-<version>-1.noarch.rpm is the version plus 17 bytes.
		{"1" + strings.Repeat("0", format.MaxPoolFileName-18), true},
		{"1" + strings.Repeat("0", format.MaxPoolFileName-17), false},
	} {
		data := fakeRPM(map[int32]string{tagName: "foo", tagVersion: tc.version, tagRelease: "1",
			tagArch: "noarch", tagSourceRPM: "foo-1-1.src.rpm"})
		path := filepath.Join(t.TempDir(), "p.rpm")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		_, err = (&Format{}).Parse(f)
		f.Close()
		if tc.ok && err != nil {
			t.Errorf("version of %d bytes rejected: %v", len(tc.version), err)
		}
		if !tc.ok && (err == nil || !strings.Contains(err.Error(), "pool file name")) {
			t.Errorf("version of %d bytes: err = %v, want a pool file name error", len(tc.version), err)
		}
	}
}
