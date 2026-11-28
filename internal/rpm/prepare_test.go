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
			if out, err := checksig(t, tools["rpmkeys"], k, signed); err != nil || !signatureOK(out) {
				t.Errorf("rpmkeys --checksig: %v\n%s", err, out)
			}
		})
	}
}

// An rpm its vendor already signed must come out with garner's signature
// only, so a client that trusts only garner's key accepts it. rpmsign
// --addsign keeps or refuses the vendor's signature on rpm 6 and EL 10.
func TestPrepareReplacesUpstreamSignature(t *testing.T) {
	tools := map[string]string{}
	for _, name := range []string{"rpmsign", "rpmkeys", "gpg", "gpgconf"} {
		p, err := exec.LookPath(name)
		if err != nil {
			t.Skipf("%s not installed", name)
		}
		tools[name] = p
	}
	dir := t.TempDir()
	key := func(name string) *signing.Keyring {
		path := filepath.Join(dir, name+".asc")
		if _, err := signing.GenerateBits(path, name, name+"@example.com", 2048); err != nil {
			t.Fatal(err)
		}
		k, err := signing.Load([]signing.KeySource{{KeyFile: path}})
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	prepare := func(k *signing.Keyring, in, name string) string {
		f := &Format{Signer: func() signing.Signer { return k },
			rpmsign: tools["rpmsign"], gpg: tools["gpg"], gpgconf: tools["gpgconf"]}
		tmp := filepath.Join(dir, name)
		if err := os.Mkdir(tmp, 0o700); err != nil {
			t.Fatal(err)
		}
		out, err := f.Prepare(context.Background(), in, tmp)
		if err != nil {
			t.Fatalf("Prepare with the %s key: %v", name, err)
		}
		return out
	}
	vendor, garner := key("vendor"), key("garner")
	vendorSigned := prepare(vendor, filepath.Join("testdata", "garner-hello-1.0-1.noarch.rpm"), "vendor")
	served := prepare(garner, vendorSigned, "garner")

	out, err := checksig(t, tools["rpmkeys"], garner, served)
	if err != nil || !signatureOK(out) || strings.Contains(out, "NOKEY") {
		t.Errorf("a client with only garner's key does not accept the re-signed rpm: %v\n%s", err, out)
	}
}

// gpg's home must stay out of tmpDir, which is under the storage directory
// and may be on NFS: there its keyring and sockets cost round trips, and
// files gpg-agent still holds open block the directory's removal.
func TestPrepareKeepsGPGHomeOutOfTmpDir(t *testing.T) {
	tools := map[string]string{}
	for _, name := range []string{"rpmsign", "gpg", "gpgconf"} {
		p, err := exec.LookPath(name)
		if err != nil {
			t.Skipf("%s not installed", name)
		}
		tools[name] = p
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
	local := filepath.Join(dir, "local")
	if err := os.Mkdir(local, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", local)
	tmp := filepath.Join(dir, "prepare")
	if err := os.Mkdir(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	f := &Format{Signer: func() signing.Signer { return k },
		rpmsign: tools["rpmsign"], gpg: tools["gpg"], gpgconf: tools["gpgconf"]}
	signed, err := f.Prepare(context.Background(), filepath.Join("testdata", "garner-hello-1.0-1.noarch.rpm"), tmp)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if signed != filepath.Join(tmp, "package.rpm") {
		t.Errorf("signed package at %s, want it in tmpDir", signed)
	}
	for d, want := range map[string]int{tmp: 1, local: 0} {
		entries, err := os.ReadDir(d)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != want {
			var names []string
			for _, e := range entries {
				names = append(names, e.Name())
			}
			t.Errorf("%s after Prepare holds %v, want %d entries", d, names, want)
		}
	}
}

// checksig runs rpmkeys --checksig -v on file with only k's public keys
// imported, and returns its output.
func checksig(t *testing.T, rpmkeys string, k *signing.Keyring, file string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	armored, _, err := k.PublicKeys()
	if err != nil {
		t.Fatal(err)
	}
	pub := filepath.Join(dir, "pub.asc")
	if err := os.WriteFile(pub, armored, 0o644); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(dir, "rpmdb")
	if out, err := exec.Command(rpmkeys, "--dbpath", db, "--import", pub).CombinedOutput(); err != nil {
		t.Fatalf("rpmkeys --import: %v\n%s", err, out)
	}
	out, err := exec.Command(rpmkeys, "--dbpath", db, "--checksig", "-v", file).CombinedOutput()
	return string(out), err
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
