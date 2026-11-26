package deb

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avesst/garner/internal/config"
	"github.com/avesst/garner/internal/format"
)

// stored parses a .deb with the given version through Format.Parse and
// returns the package as the store keeps it: epoch and version apart.
func stored(t *testing.T, version string) *format.Package {
	t.Helper()
	control := strings.Replace(testControl, "Version: 1:1.4.0-1", "Version: "+version, 1)
	path := filepath.Join(t.TempDir(), "p.deb")
	if err := os.WriteFile(path, buildDeb(t, debMember{"control.tar", controlTar(t, "./control", control)}), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	p, err := Format{}.Parse(f)
	if err != nil {
		t.Fatal(err)
	}
	return &format.Package{Name: p.Name, Epoch: p.Epoch, Version: p.Version}
}

// Stored versions must compare as dpkg compares the originals. Expected
// orders are from dpkg --compare-versions.
func TestFormatCompareStoredVersions(t *testing.T) {
	tests := []struct {
		older, newer string
	}{
		{"1.0-1", "1.0-2"},
		{"1.0", "1:0.1"},
		// A colon in the upstream part is allowed when an epoch is given.
		{"1:2:9-1", "1:5:0-1"},
		{"1:9:0", "1:10:0"},
	}
	for _, tt := range tests {
		older, newer := stored(t, tt.older), stored(t, tt.newer)
		if got := (Format{}).Compare(older, newer); got != -1 {
			t.Errorf("Compare(%s, %s) = %d, want -1", tt.older, tt.newer, got)
		}
		if got := (Format{}).Compare(newer, older); got != 1 {
			t.Errorf("Compare(%s, %s) = %d, want 1", tt.newer, tt.older, got)
		}
	}
}

// The .sources file carries the keys inline, so clients install no keyring
// file, which _apt could not read under a restrictive umask.
func TestClientKeysInline(t *testing.T) {
	keys := "-----BEGIN PGP PUBLIC KEY BLOCK-----\n\nmQINBGabc\n=wxyz\n-----END PGP PUBLIC KEY BLOCK-----\n"
	c := &config.Config{Server: config.Server{ClientName: "garner"},
		Deb: &config.Deb{Suites: []string{"stable", "testing"}}}
	clients := (Format{}).Clients(c, "https://repo.example.com", []byte(keys))
	if len(clients) != 2 {
		t.Fatalf("got %d client files, want 2", len(clients))
	}
	for _, cl := range clients {
		p, err := ParseParagraph(cl.Content)
		if err != nil {
			t.Fatalf("%s does not parse: %v\n%s", cl.Path, err, cl.Content)
		}
		v, _ := p.Get("Signed-By")
		var got []string
		for _, line := range strings.Split(v, "\n")[1:] {
			line = strings.TrimPrefix(line, " ")
			if line == "." {
				line = ""
			}
			got = append(got, line)
		}
		if strings.Join(got, "\n")+"\n" != keys {
			t.Errorf("%s: Signed-By holds\n%q\nwant\n%q", cl.Path, strings.Join(got, "\n"), keys)
		}
		if strings.Contains(cl.Instructions, "keyrings") || !strings.Contains(cl.Instructions, "sudo curl -fsSLo /etc/apt/sources.list.d/garner.sources https://repo.example.com/"+cl.Path) {
			t.Errorf("%s: instructions:\n%s", cl.Path, cl.Instructions)
		}
	}
}
