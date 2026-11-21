package rpm

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avesst/garner/internal/signing"
	"github.com/avesst/garner/internal/signing/signingtest"
)

// rpmsign gets the key through ArmoredPrivateKey and gpg --import. A key
// whose primary is kept offline must sign there too, protected or not.
func TestPrepareSignsWithSubkeyExport(t *testing.T) {
	rpmkeys, err := exec.LookPath("rpmkeys")
	if err != nil {
		t.Skip("rpmkeys not installed")
	}
	for _, pass := range []string{"secret", ""} {
		name := "protected"
		if pass == "" {
			name = "unprotected"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			key, _ := signingtest.SubkeyExport(t, dir, pass)
			src := signing.KeySource{KeyFile: key}
			if pass != "" {
				src.PassphraseFile = filepath.Join(dir, "pass")
				if err := os.WriteFile(src.PassphraseFile, []byte(pass+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			k, err := signing.Load([]signing.KeySource{src})
			if err != nil {
				t.Fatal(err)
			}
			f, err := NewFormat(func() signing.Signer { return k })
			if err != nil {
				t.Skipf("rpm tools: %v", err)
			}
			tmp := filepath.Join(dir, "prepare")
			if err := os.Mkdir(tmp, 0o700); err != nil {
				t.Fatal(err)
			}
			signed, err := f.Prepare(context.Background(), filepath.Join("testdata", "garner-hello-1.0-1.noarch.rpm"), tmp)
			if err != nil {
				t.Fatalf("Prepare: %v", err)
			}

			dbpath := filepath.Join(dir, "rpmdb")
			armored, _, err := k.PublicKeys()
			if err != nil {
				t.Fatal(err)
			}
			pubAsc := filepath.Join(dir, "pub.asc")
			if err := os.WriteFile(pubAsc, armored, 0o644); err != nil {
				t.Fatal(err)
			}
			if out, err := exec.Command(rpmkeys, "--dbpath", dbpath, "--import", pubAsc).CombinedOutput(); err != nil {
				t.Fatalf("rpmkeys --import: %v\n%s", err, out)
			}
			out, err := exec.Command(rpmkeys, "--dbpath", dbpath, "--checksig", "-v", signed).CombinedOutput()
			if err != nil || !signatureOK(string(out)) {
				t.Errorf("rpmkeys --checksig: %v\n%s", err, out)
			}
		})
	}
}

// signatureOK reports whether rpmkeys -v found a signature that verified.
// rpm versions word the line differently ("key ID", "key fingerprint").
func signatureOK(out string) bool {
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "signature") && strings.HasSuffix(strings.TrimSpace(line), ": OK") {
			return true
		}
	}
	return false
}
