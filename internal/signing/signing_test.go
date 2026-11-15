package signing

import (
	"bytes"
	"crypto"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// testKey generates a 2048-bit key, which is much faster than the 4096-bit
// default and exercises the same code.
func testKey(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name+".asc")
	cfg := &packet.Config{DefaultHash: crypto.SHA512, RSABits: 2048, Algorithm: packet.PubKeyAlgoRSA}
	if _, err := generate(path, name, name+"@example.com", cfg); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSignaturesVerifyWithGpgv(t *testing.T) {
	gpgv, err := exec.LookPath("gpgv")
	if err != nil {
		t.Skip("gpgv not installed")
	}
	dir := t.TempDir()
	k, err := Load([]KeySource{{KeyFile: testKey(t, dir, "first")}, {KeyFile: testKey(t, dir, "second")}})
	if err != nil {
		t.Fatal(err)
	}
	_, bin, err := k.PublicKeys()
	if err != nil {
		t.Fatal(err)
	}
	keyring := filepath.Join(dir, "keyring.gpg")
	write(t, keyring, bin)

	data := []byte("Origin: garner\nSuite: stable\n -dash line\n")
	var inRelease bytes.Buffer
	if err := k.ClearSign(&inRelease, data); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "InRelease"), inRelease.Bytes())
	runGpgv(t, gpgv, keyring, filepath.Join(dir, "InRelease"))

	var sig bytes.Buffer
	if err := k.DetachSign(&sig, data); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "Release"), data)
	write(t, filepath.Join(dir, "Release.gpg"), sig.Bytes())
	runGpgv(t, gpgv, keyring, filepath.Join(dir, "Release.gpg"), filepath.Join(dir, "Release"))

	// A tampered file must fail.
	write(t, filepath.Join(dir, "Release"), append(data, 'x'))
	if out, err := exec.Command(gpgv, "--keyring", keyring, filepath.Join(dir, "Release.gpg"), filepath.Join(dir, "Release")).CombinedOutput(); err == nil {
		t.Errorf("gpgv accepted a tampered file:\n%s", out)
	}
}

func runGpgv(t *testing.T, gpgv, keyring string, args ...string) {
	t.Helper()
	out, err := exec.Command(gpgv, append([]string{"--keyring", keyring}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("gpgv %v: %v\n%s", args, err, out)
	}
	if !strings.Contains(string(out), "SHA512") && !strings.Contains(string(out), "Good signature") {
		t.Logf("gpgv output:\n%s", out)
	}
}

func TestPublicKeysContainAllKeys(t *testing.T) {
	dir := t.TempDir()
	k, err := Load([]KeySource{{KeyFile: testKey(t, dir, "a")}, {KeyFile: testKey(t, dir, "b")}})
	if err != nil {
		t.Fatal(err)
	}
	arm, _, err := k.PublicKeys()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(arm, []byte("-----BEGIN PGP PUBLIC KEY BLOCK-----")) {
		t.Errorf("armored keyring starts with %q", arm[:40])
	}
	if bytes.Contains(arm, []byte("PRIVATE")) {
		t.Error("public keyring mentions a private key")
	}
	if len(k.Fingerprint()) != 40 {
		t.Errorf("Fingerprint = %q, want 40 hex digits", k.Fingerprint())
	}
}

// go-crypto keeps user IDs in a map, so its Serialize writes them in a
// random order. The published keys and the snapshot inputs digest need the
// same bytes every time.
func TestPublicKeysAreDeterministic(t *testing.T) {
	cfg := config()
	cfg.RSABits = 1024
	e, err := openpgp.NewEntity("garner", "repository signing key", "", cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"b", "c", "a"} {
		if err := e.AddUserId(name, "", "", cfg); err != nil {
			t.Fatal(err)
		}
	}
	k := &Keyring{entities: openpgp.EntityList{e}}
	_, first, err := k.PublicKeys()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if _, bin, _ := k.PublicKeys(); !bytes.Equal(bin, first) {
			t.Fatal("PublicKeys returned different bytes for the same key")
		}
	}
	// Nothing is lost: the result parses back with every user ID.
	back, err := openpgp.ReadKeyRing(bytes.NewReader(first))
	if err != nil || len(back) != 1 || len(back[0].Identities) != 4 {
		t.Fatalf("read back %d keys, err %v", len(back), err)
	}
}

func TestLoadErrors(t *testing.T) {
	dir := t.TempDir()
	priv := testKey(t, dir, "k")
	k, err := Load([]KeySource{{KeyFile: priv}})
	if err != nil {
		t.Fatal(err)
	}
	_, bin, _ := k.PublicKeys()
	pub := filepath.Join(dir, "pub.gpg")
	write(t, pub, bin)

	if _, err := Load(nil); err == nil {
		t.Error("Load(nil) succeeded")
	}
	if _, err := Load([]KeySource{{KeyFile: pub}}); err == nil || !strings.Contains(err.Error(), "private key") {
		t.Errorf("public-only first key: err = %v", err)
	}
	if _, err := Load([]KeySource{{KeyFile: priv}, {KeyFile: pub}}); err != nil {
		t.Errorf("public-only second key: %v", err)
	}
	if _, err := Load([]KeySource{{KeyFile: filepath.Join(dir, "missing")}}); err == nil {
		t.Error("missing file accepted")
	}
}

func TestGenerateRefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := testKey(t, dir, "k")
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("key file mode = %v, want 0600", st.Mode().Perm())
	}
	if _, err := Generate(path, "x", "x@example.com"); err == nil {
		t.Error("Generate overwrote an existing key")
	}
}

func TestEncryptedKeyNeedsPassphrase(t *testing.T) {
	gpg, err := exec.LookPath("gpg")
	if err != nil {
		t.Skip("gpg not installed")
	}
	dir := t.TempDir()
	home := filepath.Join(dir, "gnupg")
	os.Mkdir(home, 0o700)
	gen := exec.Command(gpg, "--homedir", home, "--batch", "--pinentry-mode", "loopback", "--passphrase", "secret",
		"--quick-generate-key", "enc@example.com", "rsa2048", "sign", "never")
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("gpg: %v\n%s", err, out)
	}
	key := filepath.Join(dir, "enc.asc")
	exp := exec.Command(gpg, "--homedir", home, "--batch", "--pinentry-mode", "loopback", "--passphrase", "secret",
		"--armor", "--output", key, "--export-secret-keys", "enc@example.com")
	if out, err := exp.CombinedOutput(); err != nil {
		t.Fatalf("gpg export: %v\n%s", err, out)
	}
	if _, err := Load([]KeySource{{KeyFile: key}}); err == nil || !strings.Contains(err.Error(), "passphrase_file") {
		t.Errorf("no passphrase: err = %v", err)
	}
	pass := filepath.Join(dir, "pass")
	write(t, pass, []byte("secret\n"))
	k, err := Load([]KeySource{{KeyFile: key, PassphraseFile: pass}})
	if err != nil {
		t.Fatal(err)
	}
	var sig bytes.Buffer
	if err := k.DetachSign(&sig, []byte("x")); err != nil {
		t.Errorf("sign with decrypted key: %v", err)
	}
}

func write(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
