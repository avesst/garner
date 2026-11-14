// Package blob is garner's content-addressed file store (design sections 4
// and 7): every package file is kept once, under its sha256.
package blob

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// Store keeps blobs under root/sha256/<first two hex digits>/<sha256>.
// Partial writes go to root/tmp, on the same filesystem, so that moving a
// finished blob into place is atomic.
type Store struct {
	root string
}

// Blob describes a stored file.
type Blob struct {
	SHA256 string // lowercase hex
	Size   int64
	// Existed is true when the store already had this content; the new copy
	// was discarded.
	Existed bool
}

// Open returns the store rooted at root, creating its directories.
func Open(root string) (*Store, error) {
	for _, d := range []string{"sha256", "tmp"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			return nil, err
		}
	}
	return &Store{root: root}, nil
}

// Put copies r into the store. The data is fsynced before it becomes
// visible under its hash, and storing content that is already present
// leaves the existing file untouched.
func (s *Store) Put(r io.Reader) (Blob, error) {
	tmp, err := os.CreateTemp(filepath.Join(s.root, "tmp"), "put-*")
	if err != nil {
		return Blob{}, err
	}
	defer os.Remove(tmp.Name()) // a no-op once the blob is linked in place

	h := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, h), r)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return Blob{}, fmt.Errorf("write blob: %w", err)
	}
	b := Blob{SHA256: hex.EncodeToString(h.Sum(nil)), Size: size}

	final := s.path(b.SHA256)
	dir := filepath.Dir(final)
	if err := s.mkdir(dir); err != nil {
		return Blob{}, err
	}
	// Link, unlike rename, fails when the target exists, so an existing
	// blob (and any pool hardlinks to it) keeps its inode.
	switch err := os.Link(tmp.Name(), final); {
	case errors.Is(err, fs.ErrExist):
		b.Existed = true
		return b, nil
	case err != nil:
		return Blob{}, fmt.Errorf("store blob: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return Blob{}, err
	}
	return b, nil
}

// Path returns the file name of the blob with the given sha256, or an
// error if sha is not a lowercase hex sha256.
func (s *Store) Path(sha string) (string, error) {
	if !validSHA256(sha) {
		return "", fmt.Errorf("invalid sha256 %q", sha)
	}
	return s.path(sha), nil
}

// Open opens the blob with the given sha256 for reading.
func (s *Store) Open(sha string) (*os.File, error) {
	p, err := s.Path(sha)
	if err != nil {
		return nil, err
	}
	return os.Open(p)
}

func (s *Store) path(sha string) string {
	return filepath.Join(s.root, "sha256", sha[:2], sha)
}

// mkdir creates a fan-out directory and makes its entry durable, so a
// blob linked into it survives a crash.
func (s *Store) mkdir(dir string) error {
	err := os.Mkdir(dir, 0o755)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return syncDir(filepath.Dir(dir))
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("sync %s: %w", dir, err)
	}
	return nil
}

func validSHA256(s string) bool {
	if len(s) != sha256.Size*2 {
		return false
	}
	for _, c := range []byte(s) {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
