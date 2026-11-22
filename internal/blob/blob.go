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
	b, f, err := s.PutOpen(r)
	if f != nil {
		f.Close()
	}
	return b, err
}

// PutOpen is Put that also returns the content opened for reading. The
// file stays readable after GC removes the blob, so it can restore it
// (Ensure).
func (s *Store) PutOpen(r io.Reader) (Blob, *os.File, error) {
	tmp, err := os.CreateTemp(filepath.Join(s.root, "tmp"), "put-*")
	if err != nil {
		return Blob{}, nil, err
	}
	defer os.Remove(tmp.Name()) // a no-op once the blob is linked in place

	// CreateTemp makes the file 0600. Pool entries are hardlinks to it, so
	// a proxy serving public/ from disk through the group needs it
	// world-readable, like the indexes (contrib/garner.tmpfiles).
	err = tmp.Chmod(0o644)
	h := sha256.New()
	var size int64
	if err == nil {
		size, err = io.Copy(io.MultiWriter(tmp, h), r)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return Blob{}, nil, fmt.Errorf("write blob: %w", err)
	}
	b := Blob{SHA256: hex.EncodeToString(h.Sum(nil)), Size: size}
	f, err := os.Open(tmp.Name())
	if err != nil {
		return Blob{}, nil, err
	}
	if err := s.link(tmp.Name(), &b); err != nil {
		f.Close()
		return Blob{}, nil, err
	}
	return b, f, nil
}

// link moves the finished temp file into place as blob b.
func (s *Store) link(tmp string, b *Blob) error {
	final := s.path(b.SHA256)
	dir := filepath.Dir(final)
	if err := s.mkdir(dir); err != nil {
		return err
	}
	// Link, unlike rename, fails when the target exists, so an existing
	// blob (and any pool hardlinks to it) keeps its inode.
	switch err := os.Link(tmp, final); {
	case errors.Is(err, fs.ErrExist):
		b.Existed = true
		return nil
	case err != nil:
		return fmt.Errorf("store blob: %w", err)
	}
	return syncDir(dir)
}

// Ensure stores the blob again from src, which holds its size bytes, when
// its file is missing. Ingest calls it in the transaction that records the
// blob, because GC may have removed the file after Put found it present.
// src can be a file opened before the removal: it stays readable.
func (s *Store) Ensure(sha string, src io.ReaderAt, size int64) error {
	p, err := s.Path(sha)
	if err != nil {
		return err
	}
	if _, err := os.Stat(p); !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	b, err := s.Put(io.NewSectionReader(src, 0, size))
	if err != nil {
		return err
	}
	if b.SHA256 != sha {
		return fmt.Errorf("restore blob %s: got content %s", sha, b.SHA256)
	}
	return nil
}

// Path returns the file name of the blob with the given sha256, or an
// error if sha is not a lowercase hex sha256.
func (s *Store) Path(sha string) (string, error) {
	if !validSHA256(sha) {
		return "", fmt.Errorf("invalid sha256 %q", sha)
	}
	return s.path(sha), nil
}

// PathOf is Path for a sha256 the caller has validated.
func (s *Store) PathOf(sha string) string { return s.path(sha) }

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
