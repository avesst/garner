package signing

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
)

// PublicKeyring checks signatures against published keys, the way a client
// does with key.gpg or key.asc.
type PublicKeyring struct {
	entities openpgp.EntityList
}

// ReadPublicKeys reads a keyring, ASCII-armored or binary.
func ReadPublicKeys(data []byte) (*PublicKeyring, error) {
	var el openpgp.EntityList
	var err error
	if bytes.HasPrefix(bytes.TrimSpace(data), []byte("-----BEGIN PGP")) {
		el, err = openpgp.ReadArmoredKeyRing(bytes.NewReader(data))
	} else {
		el, err = openpgp.ReadKeyRing(bytes.NewReader(data))
	}
	if err != nil {
		return nil, err
	}
	if len(el) == 0 {
		return nil, errors.New("no keys")
	}
	return &PublicKeyring{entities: el}, nil
}

// Fingerprints returns the primary keys' fingerprints, upper-case hex,
// sorted.
func (k *PublicKeyring) Fingerprints() []string {
	out := make([]string, len(k.entities))
	for i, e := range k.entities {
		out[i] = strings.ToUpper(fmt.Sprintf("%x", e.PrimaryKey.Fingerprint))
	}
	slices.Sort(out)
	return out
}

// VerifyDetached checks a detached signature over data, armored or binary.
func (k *PublicKeyring) VerifyDetached(data, sig []byte) error {
	var err error
	if bytes.HasPrefix(bytes.TrimSpace(sig), []byte("-----BEGIN PGP")) {
		_, err = openpgp.CheckArmoredDetachedSignature(k.entities, bytes.NewReader(data), bytes.NewReader(sig), nil)
	} else {
		_, err = openpgp.CheckDetachedSignature(k.entities, bytes.NewReader(data), bytes.NewReader(sig), nil)
	}
	return err
}

// VerifyClearSigned checks a cleartext-signed message and returns the text
// it signs.
func (k *PublicKeyring) VerifyClearSigned(msg []byte) ([]byte, error) {
	b, rest := clearsign.Decode(msg)
	if b == nil {
		return nil, errors.New("not a cleartext-signed message")
	}
	if len(bytes.TrimSpace(rest)) > 0 {
		return nil, errors.New("data after the signature")
	}
	if _, err := b.VerifySignature(k.entities, nil); err != nil {
		return nil, err
	}
	return b.Plaintext, nil
}
