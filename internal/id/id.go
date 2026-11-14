// Package id generates the identifiers garner stores in TEXT id columns
// and uses as snapshot directory names: ULIDs, 26 characters of Crockford
// base32 that sort by creation time (https://github.com/ulid/spec).
package id

import (
	"crypto/rand"
	"encoding/binary"
	"time"
)

const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// New returns a ULID for the current time.
func New() string {
	return newAt(time.Now())
}

// newAt encodes 48 bits of Unix milliseconds followed by 80 random bits.
// IDs made within the same millisecond do not sort by creation order.
func newAt(t time.Time) string {
	var b [16]byte
	var ms [8]byte
	binary.BigEndian.PutUint64(ms[:], uint64(t.UnixMilli()))
	copy(b[:6], ms[2:])
	rand.Read(b[6:]) // never returns an error (crypto/rand, Go 1.24+)
	return encode(b)
}

// encode writes 128 bits as 26 base32 digits, most significant first; the
// first digit carries only the top 3 bits.
func encode(b [16]byte) string {
	hi := binary.BigEndian.Uint64(b[:8])
	lo := binary.BigEndian.Uint64(b[8:])
	var out [26]byte
	for i := 25; i >= 0; i-- {
		out[i] = alphabet[lo&31]
		lo = lo>>5 | hi<<59
		hi >>= 5
	}
	return string(out[:])
}
