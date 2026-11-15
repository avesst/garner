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
	cs := f.Clients(cfg, "https://repo.example.com")
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
