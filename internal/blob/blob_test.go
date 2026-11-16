package blob

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPutAndOpen(t *testing.T) {
	s := newStore(t)
	content := "package contents"
	b, err := s.Put(strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(content))
	if want := hex.EncodeToString(sum[:]); b.SHA256 != want {
		t.Errorf("SHA256 = %s, want %s", b.SHA256, want)
	}
	if b.Size != int64(len(content)) || b.Existed {
		t.Errorf("Put = %+v, want size %d and Existed false", b, len(content))
	}

	f, err := s.Open(b.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, err := io.ReadAll(f)
	if err != nil || string(got) != content {
		t.Errorf("read back %q, %v; want %q", got, err, content)
	}
	if want := filepath.Join("sha256", b.SHA256[:2], b.SHA256); !strings.HasSuffix(f.Name(), want) {
		t.Errorf("path %s does not end in %s", f.Name(), want)
	}
}

func TestPutTwiceIsNoOp(t *testing.T) {
	s := newStore(t)
	first, err := s.Put(strings.NewReader("same"))
	if err != nil {
		t.Fatal(err)
	}
	p, _ := s.Path(first.SHA256)
	before, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}

	second, err := s.Put(strings.NewReader("same"))
	if err != nil {
		t.Fatal(err)
	}
	if !second.Existed || second.SHA256 != first.SHA256 {
		t.Errorf("second Put = %+v, want Existed with sha %s", second, first.SHA256)
	}
	after, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Error("second Put replaced the existing file")
	}
	assertTmpEmpty(t, s)
}

func TestPutReadErrorLeavesNothing(t *testing.T) {
	s := newStore(t)
	r := io.MultiReader(strings.NewReader("partial"), errReader{})
	if _, err := s.Put(r); err == nil {
		t.Fatal("Put succeeded on a failing reader")
	}
	assertTmpEmpty(t, s)
	entries, err := os.ReadDir(filepath.Join(s.root, "sha256"))
	if err != nil || len(entries) != 0 {
		t.Errorf("sha256/ has %d entries, %v; want none", len(entries), err)
	}
}

func TestPathRejectsInvalidHash(t *testing.T) {
	s := newStore(t)
	for _, sha := range []string{
		"", "abc", "../../etc/passwd",
		strings.Repeat("A", 64), strings.Repeat("g", 64), strings.Repeat("a", 65),
	} {
		if _, err := s.Path(sha); err == nil {
			t.Errorf("Path(%q) succeeded", sha)
		}
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func assertTmpEmpty(t *testing.T, s *Store) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(s.root, "tmp"))
	if err != nil || len(entries) != 0 {
		t.Errorf("tmp/ has %d entries, %v; want none", len(entries), err)
	}
}

func TestEnsureRestoresFromOpenFile(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b, f, err := s.PutOpen(strings.NewReader("content"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	path, _ := s.Path(b.SHA256)
	if err := os.Remove(path); err != nil { // as GC would
		t.Fatal(err)
	}
	if err := s.Ensure(b.SHA256, f, b.Size); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "content" {
		t.Errorf("restored blob = %q, %v", got, err)
	}
	// Present: left alone. Wrong content: refused.
	if err := s.Ensure(b.SHA256, strings.NewReader("other"), 5); err != nil {
		t.Errorf("Ensure of a present blob: %v", err)
	}
	os.Remove(path)
	if err := s.Ensure(b.SHA256, strings.NewReader("other"), 5); err == nil {
		t.Error("Ensure stored content with another hash")
	}
}
