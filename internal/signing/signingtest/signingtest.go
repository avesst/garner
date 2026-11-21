// Package signingtest builds signing keys with gpg for tests.
package signingtest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// SubkeyExport makes a key the way an offline primary is usually kept: a
// certify-only primary and a signing subkey, exported with
// --export-secret-subkeys, so the file holds a stub in place of the
// primary's secret. pass protects the subkey; "" leaves it unprotected.
// It returns the armored secret key file and the binary public keyring,
// and skips the test when gpg is missing.
func SubkeyExport(t *testing.T, dir, pass string) (keyFile, pubKeyring string) {
	t.Helper()
	return export(t, dir, pass, "cert", "sign")
}

// StubSigner is SubkeyExport for a key whose primary signs and whose
// subkey only encrypts: the file has no usable secret for signing.
func StubSigner(t *testing.T, dir string) (keyFile, pubKeyring string) {
	t.Helper()
	return export(t, dir, "", "sign", "encr")
}

func export(t *testing.T, dir, pass, primaryUsage, subUsage string) (keyFile, pubKeyring string) {
	t.Helper()
	gpg, err := exec.LookPath("gpg")
	if err != nil {
		t.Skip("gpg not installed")
	}
	home := filepath.Join(dir, "gnupg-offline")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		base := []string{"--homedir", home, "--batch", "--pinentry-mode", "loopback", "--passphrase", pass}
		out, err := exec.Command(gpg, append(base, args...)...).Output()
		if err != nil {
			stderr := ""
			if ee, ok := err.(*exec.ExitError); ok {
				stderr = string(ee.Stderr)
			}
			t.Fatalf("gpg %v: %v\n%s", args, err, stderr)
		}
		return string(out)
	}
	run("--quick-generate-key", "offline@example.com", "rsa2048", primaryUsage, "never")
	var fpr string
	for _, line := range strings.Split(run("--with-colons", "--list-keys", "offline@example.com"), "\n") {
		if f := strings.Split(line, ":"); f[0] == "fpr" {
			fpr = f[9]
			break
		}
	}
	if fpr == "" {
		t.Fatal("gpg listed no fingerprint")
	}
	run("--quick-add-key", fpr, "rsa2048", subUsage, "never")
	keyFile = filepath.Join(dir, "offline.asc")
	run("--armor", "--output", keyFile, "--export-secret-subkeys", fpr)
	pubKeyring = filepath.Join(dir, "offline.gpg")
	run("--output", pubKeyring, "--export", fpr)
	exec.Command("gpgconf", "--homedir", home, "--kill", "all").Run()
	return keyFile, pubKeyring
}
