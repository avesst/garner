package fsutil

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestDirSyncRecordsChangedDirectories(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "pool"), 0o755); err != nil {
		t.Fatal(err)
	}
	var s DirSync
	dst := filepath.Join(root, "pool", "a", "b", "file")
	if err := s.LinkOrCopy(src, dst); err != nil {
		t.Fatal(err)
	}
	// b holds the link; a and pool gained the new directories b and a.
	want := []string{filepath.Join(root, "pool"), filepath.Join(root, "pool", "a"), filepath.Join(root, "pool", "a", "b")}
	var got []string
	for d := range s.dirs {
		got = append(got, d)
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("recorded %v, want %v", got, want)
	}
	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}
	if s.dirs != nil {
		t.Error("Sync kept the recorded directories")
	}

	// A link that already exists changes nothing.
	if err := s.LinkOrCopy(src, dst); err != nil {
		t.Fatal(err)
	}
	if len(s.dirs) != 0 {
		t.Errorf("existing link recorded %v", s.dirs)
	}
}
