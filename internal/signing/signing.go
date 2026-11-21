// Package signing holds garner's repository keys and signs metadata with
// the first of them (design section 10).
package signing

import (
	"bytes"
	"crypto"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// Signer signs repository metadata. It is an interface so that HSM or KMS
// backends can be added later.
type Signer interface {
	// ClearSign writes data as a cleartext-signed message (InRelease).
	ClearSign(w io.Writer, data []byte) error
	// DetachSign writes an ASCII-armored detached signature over data
	// (Release.gpg, repomd.xml.asc).
	DetachSign(w io.Writer, data []byte) error
	// PublicKeys returns every published key, armored and binary.
	PublicKeys() (armored, binary []byte, err error)
	// Fingerprint is the signing key's fingerprint, upper-case hex.
	Fingerprint() string
}

// KeyExporter is implemented by signers whose private key can be handed to
// an external tool. rpmsign needs it (section 10); an HSM signer would not
// implement it, which rules out rpm re-signing with that backend.
type KeyExporter interface {
	ArmoredPrivateKey() ([]byte, error)
}

// config pins the algorithms: SHA-512 for signatures, because the clients
// garner supports reject SHA-1 (section 4).
func config() *packet.Config {
	return &packet.Config{
		DefaultHash: crypto.SHA512,
		RSABits:     4096,
		Algorithm:   packet.PubKeyAlgoRSA,
	}
}

// Keyring is the configured keys; the first one signs.
type Keyring struct {
	entities openpgp.EntityList
}

// KeySource is one key file and its optional passphrase file.
type KeySource struct {
	KeyFile        string
	PassphraseFile string
}

// Load reads the key files. The first must contain a private key; the
// others may be public only, which is enough to publish them.
func Load(keys []KeySource) (*Keyring, error) {
	if len(keys) == 0 {
		return nil, errors.New("no signing keys configured")
	}
	k := &Keyring{}
	for i, src := range keys {
		e, err := loadKey(src, i == 0)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", src.KeyFile, err)
		}
		k.entities = append(k.entities, e)
	}
	return k, nil
}

func loadKey(src KeySource, needPrivate bool) (*openpgp.Entity, error) {
	data, err := os.ReadFile(src.KeyFile)
	if err != nil {
		return nil, err
	}
	var list openpgp.EntityList
	if bytes.Contains(data, []byte("-----BEGIN PGP")) {
		list, err = openpgp.ReadArmoredKeyRing(bytes.NewReader(data))
	} else {
		list, err = openpgp.ReadKeyRing(bytes.NewReader(data))
	}
	if err != nil {
		return nil, fmt.Errorf("read key: %w", err)
	}
	if len(list) != 1 {
		return nil, fmt.Errorf("file holds %d keys, want 1", len(list))
	}
	e := list[0]
	if e.PrivateKey == nil {
		if needPrivate {
			return nil, errors.New("the first key signs and must include its private key")
		}
		return e, nil
	}
	if encrypted(e) {
		if src.PassphraseFile == "" {
			return nil, errors.New("private key is encrypted and no passphrase_file is set")
		}
		pass, err := os.ReadFile(src.PassphraseFile)
		if err != nil {
			return nil, err
		}
		pass = bytes.TrimRight(pass, "\r\n")
		if err := e.DecryptPrivateKeys(pass); err != nil {
			return nil, fmt.Errorf("decrypt private key: %w", err)
		}
	}
	if !needPrivate {
		return e, nil
	}
	sk, ok := e.SigningKey(time.Now())
	if !ok {
		return nil, errors.New("key has no valid signing key (expired or revoked?)")
	}
	// The signing key is often a subkey, and its secret can be absent even
	// when the primary's is there, or the other way round.
	if sk.PrivateKey == nil || sk.PrivateKey.Dummy() {
		return nil, fmt.Errorf("the secret of signing key %X is not in the file", sk.PublicKey.KeyId)
	}
	if sk.PrivateKey.Encrypted {
		return nil, fmt.Errorf("signing key %X is still encrypted", sk.PublicKey.KeyId)
	}
	return e, nil
}

// encrypted reports whether any secret key in e that is present needs a
// passphrase. With the primary kept offline, only subkeys are present.
func encrypted(e *openpgp.Entity) bool {
	keys := []*packet.PrivateKey{e.PrivateKey}
	for _, s := range e.Subkeys {
		keys = append(keys, s.PrivateKey)
	}
	for _, k := range keys {
		if k != nil && !k.Dummy() && k.Encrypted {
			return true
		}
	}
	return false
}

func (k *Keyring) signer() *openpgp.Entity { return k.entities[0] }

func (k *Keyring) ClearSign(w io.Writer, data []byte) error {
	sk, ok := k.signer().SigningKey(time.Now())
	if !ok {
		return errors.New("signing key is not usable")
	}
	pt, err := clearsign.Encode(w, sk.PrivateKey, config())
	if err != nil {
		return err
	}
	if _, err := pt.Write(data); err != nil {
		return err
	}
	return pt.Close()
}

func (k *Keyring) DetachSign(w io.Writer, data []byte) error {
	return openpgp.ArmoredDetachSign(w, k.signer(), bytes.NewReader(data), config())
}

func (k *Keyring) PublicKeys() (armored, binary []byte, err error) {
	var bin bytes.Buffer
	for _, e := range k.entities {
		if err := serializePublic(&bin, e); err != nil {
			return nil, nil, err
		}
	}
	var arm bytes.Buffer
	aw, err := armor.Encode(&arm, openpgp.PublicKeyType, nil)
	if err != nil {
		return nil, nil, err
	}
	if _, err := aw.Write(bin.Bytes()); err != nil {
		return nil, nil, err
	}
	if err := aw.Close(); err != nil {
		return nil, nil, err
	}
	arm.WriteByte('\n')
	return arm.Bytes(), bin.Bytes(), nil
}

// serializePublic writes e like Entity.Serialize, but with the user IDs in
// a fixed order: the primary one first, then by name. Serialize ranges over
// a map, so a key with several user IDs would come out differently each
// time, and so would the published key files.
func serializePublic(w io.Writer, e *openpgp.Entity) error {
	if err := e.PrimaryKey.Serialize(w); err != nil {
		return err
	}
	for _, sig := range e.Revocations {
		if err := sig.Serialize(w); err != nil {
			return err
		}
	}
	for _, sig := range e.Signatures {
		if err := sig.Serialize(w); err != nil {
			return err
		}
	}
	idents := slices.Collect(maps.Values(e.Identities))
	primary := e.PrimaryIdentity()
	slices.SortFunc(idents, func(a, b *openpgp.Identity) int {
		switch {
		case a == primary:
			return -1
		case b == primary:
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	for _, ident := range idents {
		if err := ident.UserId.Serialize(w); err != nil {
			return err
		}
		for _, sig := range ident.Signatures {
			if err := sig.Serialize(w); err != nil {
				return err
			}
		}
	}
	for _, sub := range e.Subkeys {
		if err := sub.PublicKey.Serialize(w); err != nil {
			return err
		}
		for _, sig := range sub.Revocations {
			if err := sig.Serialize(w); err != nil {
				return err
			}
		}
		if err := sub.Sig.Serialize(w); err != nil {
			return err
		}
	}
	return nil
}

func (k *Keyring) Fingerprint() string {
	return fmt.Sprintf("%X", k.signer().PrimaryKey.Fingerprint)
}

// ArmoredPrivateKey exports the signing key, decrypted, for rpmsign.
func (k *Keyring) ArmoredPrivateKey() ([]byte, error) {
	var buf bytes.Buffer
	aw, err := armor.Encode(&buf, openpgp.PrivateKeyType, nil)
	if err != nil {
		return nil, err
	}
	if err := k.signer().SerializePrivateWithoutSigning(aw, nil); err != nil {
		return nil, err
	}
	if err := aw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Generate creates an RSA 4096 key without expiry or passphrase and writes
// it, armored, to path with mode 0600. It refuses to overwrite a file.
func Generate(path, name, email string) (fingerprint string, err error) {
	return generate(path, name, email, config())
}

// GenerateBits is Generate with another RSA key size; tests use it for
// speed.
func GenerateBits(path, name, email string, bits int) (string, error) {
	cfg := config()
	cfg.RSABits = bits
	return generate(path, name, email, cfg)
}

func generate(path, name, email string, cfg *packet.Config) (string, error) {
	if _, err := os.Lstat(path); err == nil {
		return "", fmt.Errorf("%s already exists", path)
	}
	e, err := openpgp.NewEntity(name, "repository signing key", email, cfg)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	aw, err := armor.Encode(&buf, openpgp.PrivateKeyType, nil)
	if err != nil {
		return "", err
	}
	if err := e.SerializePrivate(aw, cfg); err != nil {
		return "", err
	}
	if err := aw.Close(); err != nil {
		return "", err
	}
	buf.WriteByte('\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return strings.ToUpper(fmt.Sprintf("%x", e.PrimaryKey.Fingerprint)), nil
}

// ExpiryDays returns the days until the signing key expires, and false
// for a key without expiry.
func (k *Keyring) ExpiryDays() (float64, bool) {
	e := k.signer()
	id := e.PrimaryIdentity()
	if id == nil || id.SelfSignature == nil || id.SelfSignature.KeyLifetimeSecs == nil || *id.SelfSignature.KeyLifetimeSecs == 0 {
		return 0, false
	}
	expires := e.PrimaryKey.CreationTime.Add(time.Duration(*id.SelfSignature.KeyLifetimeSecs) * time.Second)
	return time.Until(expires).Hours() / 24, true
}
