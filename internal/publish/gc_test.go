package publish

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSweepTemp(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	old := now.Add(-time.Hour)
	write := func(rel string, mtime time.Time) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(p, mtime, mtime)
	}
	age := func(rel string, mtime time.Time) { os.Chtimes(filepath.Join(root, rel), mtime, mtime) }

	write("put-old", old) // crashed upload
	write("put-new", now) // upload in progress
	// Left by a crash.
	write("sync-crashed/asset-1", old)
	write("sync-crashed/sub/asset-2", old)
	age("sync-crashed/sub", old)
	age("sync-crashed", old)
	// Downloaded long ago, directory touched since.
	write("sync-working/asset-1", old)
	age("sync-working", now)
	// Still downloading.
	write("sync-writing/asset-1", now)
	age("sync-writing", old)
	// Waiting for its first file.
	os.Mkdir(filepath.Join(root, "sync-empty"), 0o755)

	n, err := sweepTemp(root, now.Add(-10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("removed %d files, want 3", n)
	}
	for _, rel := range []string{"put-old", "sync-crashed"} {
		if _, err := os.Stat(filepath.Join(root, rel)); !os.IsNotExist(err) {
			t.Errorf("%s kept", rel)
		}
	}
	for _, rel := range []string{"put-new", "sync-working/asset-1", "sync-writing/asset-1", "sync-empty"} {
		if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
			t.Errorf("%s: %v", rel, err)
		}
	}
	if n, err := sweepTemp(filepath.Join(root, "missing"), now); n != 0 || err != nil {
		t.Errorf("missing root: %d, %v", n, err)
	}
}
