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
	// Only the tools Prepare and the check use: NewFormat would also
	// require createrepo_c.
	tools := map[string]string{}
	for _, name := range []string{"rpmsign", "rpmkeys", "gpg", "gpgconf"} {
		p, err := exec.LookPath(name)
		if err != nil {
			t.Skipf("%s not installed", name)
		}
		tools[name] = p
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
			f := &Format{Signer: func() signing.Signer { return k },
				rpmsign: tools["rpmsign"], gpg: tools["gpg"], gpgconf: tools["gpgconf"]}
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
			if out, err := exec.Command(tools["rpmkeys"], "--dbpath", dbpath, "--import", pubAsc).CombinedOutput(); err != nil {
				t.Fatalf("rpmkeys --import: %v\n%s", err, out)
			}
			out, err := exec.Command(tools["rpmkeys"], "--dbpath", dbpath, "--checksig", "-v", signed).CombinedOutput()
			if err != nil || !signatureOK(string(out)) {
				t.Errorf("rpmkeys --checksig: %v\n%s", err, out)
			}
		})
	}
}

// signatureOK reports whether rpmkeys -v found a signature that verified.
// rpm versions word the line differently: "V4 RSA/SHA512 Signature, key
// ID ...: OK" on Ubuntu 24.04, "OpenPGP V4 RSA/SHA512 signature, key
// fingerprint ...: OK" in rpm 6.0.
func signatureOK(out string) bool {
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(strings.ToLower(line), "signature") && strings.HasSuffix(strings.TrimSpace(line), ": OK") {
			return true
		}
	}
	return false
}

func TestSignatureOK(t *testing.T) {
	for out, want := range map[string]bool{
		"x.rpm:\n    Header V4 RSA/SHA512 Signature, key ID 0a7be7d4: OK\n":                                  true,
		"x.rpm:\n    Header OpenPGP V4 RSA/SHA512 signature, key fingerprint d370b497: OK\n":                 true,
		"x.rpm:\n    Header V4 RSA/SHA512 Signature, key ID 0a7be7d4: NOKEY\n    Header SHA256 digest: OK\n": false,
		"x.rpm:\n    Header SHA256 digest: OK\n":                                                             false,
	} {
		if got := signatureOK(out); got != want {
			t.Errorf("signatureOK(%q) = %v, want %v", out, got, want)
		}
	}
}
