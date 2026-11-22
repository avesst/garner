package signing

import (
	"bytes"
	"path/filepath"
	"testing"
)

func TestPublicKeyringVerifies(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key.asc")
	fp, err := GenerateBits(keyFile, "test", "test@example.com", 2048)
	if err != nil {
		t.Fatal(err)
	}
	k, err := Load([]KeySource{{KeyFile: keyFile}})
	if err != nil {
		t.Fatal(err)
	}
	armored, binary, err := k.PublicKeys()
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("Origin: test\nSuite: stable\n SHA256:\n abc 1 x\n")

	for name, keys := range map[string][]byte{"armored": armored, "binary": binary} {
		pk, err := ReadPublicKeys(keys)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if fps := pk.Fingerprints(); len(fps) != 1 || fps[0] != fp {
			t.Errorf("%s: fingerprints %v, want [%s]", name, fps, fp)
		}

		var sig bytes.Buffer
		if err := k.DetachSign(&sig, data); err != nil {
			t.Fatal(err)
		}
		if err := pk.VerifyDetached(data, sig.Bytes()); err != nil {
			t.Errorf("%s: detached: %v", name, err)
		}
		if err := pk.VerifyDetached(append([]byte("x"), data...), sig.Bytes()); err == nil {
			t.Errorf("%s: detached signature accepted over other data", name)
		}

		var clear bytes.Buffer
		if err := k.ClearSign(&clear, data); err != nil {
			t.Fatal(err)
		}
		text, err := pk.VerifyClearSigned(clear.Bytes())
		if err != nil {
			t.Fatalf("%s: clearsigned: %v", name, err)
		}
		if !bytes.Equal(text, data) {
			t.Errorf("%s: clearsigned text %q, want %q", name, text, data)
		}
		tampered := bytes.Replace(clear.Bytes(), []byte("Suite: stable"), []byte("Suite: unstab"), 1)
		if _, err := pk.VerifyClearSigned(tampered); err == nil {
			t.Errorf("%s: tampered clearsigned message accepted", name)
		}
	}

	other := filepath.Join(dir, "other.asc")
	if _, err := GenerateBits(other, "other", "o@example.com", 2048); err != nil {
		t.Fatal(err)
	}
	ok, _ := Load([]KeySource{{KeyFile: other}})
	oa, _, _ := ok.PublicKeys()
	opk, _ := ReadPublicKeys(oa)
	var sig bytes.Buffer
	k.DetachSign(&sig, data)
	if err := opk.VerifyDetached(data, sig.Bytes()); err == nil {
		t.Error("signature accepted with another key")
	}
}
