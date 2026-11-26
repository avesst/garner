package rpm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avesst/garner/internal/config"
	"github.com/avesst/garner/internal/format"
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
