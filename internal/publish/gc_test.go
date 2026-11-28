package publish

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// On NFS a removed file that a client still reads lives on as .nfs*, which
// cannot be deleted until the client closes it (EBUSY). GC skips such a
// file, finishes its other work, and removes it in a later run.
func TestGCSkipsFilesInUse(t *testing.T) {
	f := newFixture(t)
	f.cfg.Storage.SnapshotRetention = 2
	f.cfg.Storage.GCGrace = 0
	f.cfg.Defaults.Retention = 1
	f.add(t, "app", "1.0", "amd64")
	f.publish(t)
	f.add(t, "app", "2.0", "amd64")
	f.publish(t)
	f.add(t, "app", "3.0", "amd64")
	f.publish(t)

	var busy string
	removeAll = func(path string) error {
		if strings.Contains(filepath.Base(path), "app_1.0") {
			busy = path
			return &fs.PathError{Op: "unlinkat", Path: path, Err: syscall.EBUSY}
		}
		return os.RemoveAll(path)
	}
	t.Cleanup(func() { removeAll = os.RemoveAll })

	res, err := f.pub.GC(context.Background())
	if err != nil {
		t.Fatalf("GC with a file in use: %v", err)
	}
	if busy == "" {
		t.Fatal("GC did not try to remove app 1.0's pool file")
	}
	// The snapshot, and the blob that GC removes after the pool, still go.
	if len(res.Snapshots) != 1 || res.Blobs != 1 || res.PoolFiles != 0 || res.InUse != 1 {
		t.Errorf("GC = %+v, want 1 snapshot, 1 blob, no pool file and 1 file in use", res)
	}
	if _, err := os.Lstat(busy); err != nil {
		t.Errorf("the file in use: %v", err)
	}

	removeAll = os.RemoveAll
	res, err = f.pub.GC(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.PoolFiles != 1 || res.InUse != 0 {
		t.Errorf("GC after the file was closed = %+v, want 1 pool file and none in use", res)
	}
	if _, err := os.Lstat(busy); !os.IsNotExist(err) {
		t.Errorf("the file once in use still exists: %v", err)
	}
}

// Removing a directory with a file still open over NFS turns that file into
// .nfs* in the same directory, so the directory cannot be removed
// (ENOTEMPTY). GC keeps such a snapshot, with its row, for a later run.
func TestGCKeepsSnapshotInUse(t *testing.T) {
	f := newFixture(t)
	f.cfg.Storage.SnapshotRetention = 2
	f.cfg.Storage.GCGrace = 0
	f.add(t, "app", "1.0", "amd64")
	s1 := f.publish(t)
	f.add(t, "app", "2.0", "amd64")
	f.publish(t)
	f.add(t, "app", "3.0", "amd64")
	f.publish(t)

	dir := filepath.Join(f.root, "snapshots", s1.ID)
	removeAll = func(path string) error {
		if path == dir {
			return &fs.PathError{Op: "unlinkat", Path: path, Err: syscall.ENOTEMPTY}
		}
		return os.RemoveAll(path)
	}
	t.Cleanup(func() { removeAll = os.RemoveAll })

	res, err := f.pub.GC(context.Background())
	if err != nil {
		t.Fatalf("GC with a snapshot in use: %v", err)
	}
	if len(res.Snapshots) != 0 || res.InUse != 1 {
		t.Errorf("GC = %+v, want no snapshot removed and 1 in use", res)
	}
	snaps, err := f.pub.Store.Snapshots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 3 {
		t.Errorf("%d snapshots recorded, want all 3 while one is in use", len(snaps))
	}

	removeAll = os.RemoveAll
	res, err = f.pub.GC(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Snapshots) != 1 || res.Snapshots[0] != s1.ID || res.InUse != 0 {
		t.Errorf("GC after the file was closed = %+v, want %s removed", res, s1.ID)
	}
}

func TestSweepTempSkipsEntryInUse(t *testing.T) {
	root := t.TempDir()
	old := time.Now().Add(-time.Hour)
	for _, rel := range []string{"busy/a", "busy/b", "done/c"} {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(p, old, old)
		os.Chtimes(filepath.Dir(p), old, old)
	}
	removeAll = func(path string) error {
		if filepath.Base(path) == "busy" {
			return &fs.PathError{Op: "unlinkat", Path: path, Err: syscall.ENOTEMPTY}
		}
		return os.RemoveAll(path)
	}
	t.Cleanup(func() { removeAll = os.RemoveAll })

	n, inUse, err := sweepTemp(root, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || inUse != 1 {
		t.Errorf("sweepTemp = %d removed, %d in use; want 1 and 1", n, inUse)
	}
}

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

	n, _, err := sweepTemp(root, now.Add(-10*time.Minute))
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
	if n, _, err := sweepTemp(filepath.Join(root, "missing"), now); n != 0 || err != nil {
		t.Errorf("missing root: %d, %v", n, err)
	}
}
