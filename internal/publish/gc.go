package publish

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avesst/garner/internal/store"
)

// GCResult reports what a collection removed.
type GCResult struct {
	Snapshots []string `json:"snapshots"`
	Blobs     int      `json:"blobs"`
	PoolFiles int      `json:"pool_files"`
	TempFiles int      `json:"temp_files"`
}

// GC removes snapshots beyond storage.snapshot_retention whose successor
// has been current for longer than storage.gc_grace, then every package,
// pool entry and blob that no retained snapshot or live placement
// references (section 9).
func (p *Publisher) GC(ctx context.Context) (*GCResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cfg := p.Config()
	grace := cfg.Storage.GCGrace
	now := time.Now()
	res := &GCResult{}

	snaps, err := p.Store.Snapshots(ctx) // newest first
	if err != nil {
		return nil, err
	}
	for i, s := range snaps {
		if i < cfg.Storage.SnapshotRetention || s.IsCurrent {
			continue
		}
		// A snapshot is replaced when the next newer one is created;
		// requests resolved through it get gc_grace to finish.
		replaced, err := store.ParseTime(snaps[i-1].CreatedAt)
		if err != nil {
			return nil, err
		}
		if now.Sub(replaced) < grace {
			continue
		}
		if err := os.RemoveAll(filepath.Join(p.snapshotsDir(), s.ID)); err != nil {
			return nil, err
		}
		if err := p.Store.DeleteSnapshot(ctx, s.ID); err != nil {
			return nil, err
		}
		res.Snapshots = append(res.Snapshots, s.ID)
	}

	var gone []string
	err = p.Store.Tx(ctx, func(q *store.Queries) error {
		var err error
		gone, err = q.DeleteUnreferencedPackages(ctx)
		return err
	})
	if err != nil {
		return nil, err
	}

	// Pool entries live in a directory named after the first 16 hex
	// digits of the served blob (section 4). Keep those still referenced.
	live, err := p.Store.ReferencedBlobs(ctx)
	if err != nil {
		return nil, err
	}
	prefixes := map[string]bool{}
	for sha := range live {
		prefixes[sha[:16]] = true
	}
	for _, fm := range p.Formats.All() {
		root := p.poolDir(fm.Name())
		n, err := sweep(root, func(path string, d fs.DirEntry) bool {
			return !prefixes[filepath.Base(filepath.Dir(path))]
		})
		if err != nil {
			return nil, err
		}
		res.PoolFiles += n
	}

	for _, sha := range gone {
		path, err := p.Blobs.Path(sha)
		if err != nil {
			continue
		}
		if err := os.Remove(path); err == nil {
			res.Blobs++
		}
	}

	// Blob files without a database row, and temp files, older than the
	// grace period: an upload that failed after storing its file, a
	// rejected upload, or a crash.
	known, err := p.Store.Blobs(ctx)
	if err != nil {
		return nil, err
	}
	old := func(d fs.DirEntry) bool {
		info, err := d.Info()
		return err == nil && now.Sub(info.ModTime()) > grace
	}
	n, err := sweep(filepath.Join(p.Root, "blobs", "sha256"), func(path string, d fs.DirEntry) bool {
		_, ok := known[d.Name()]
		return !ok && !live[d.Name()] && old(d)
	})
	if err != nil {
		return nil, err
	}
	res.Blobs += n
	for _, dir := range []string{filepath.Join(p.Root, "blobs", "tmp"), filepath.Join(p.Root, "tmp")} {
		n, err := sweep(dir, func(path string, d fs.DirEntry) bool { return old(d) })
		if err != nil {
			return nil, err
		}
		res.TempFiles += n
	}
	entries, err := os.ReadDir(p.snapshotsDir())
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") && old(e) {
			os.RemoveAll(filepath.Join(p.snapshotsDir(), e.Name()))
			res.TempFiles++
		}
	}
	p.Log.Info("gc finished", "snapshots", len(res.Snapshots), "blobs", res.Blobs,
		"pool_files", res.PoolFiles, "temp_files", res.TempFiles)
	return res, nil
}

// sweep removes the regular files below root for which remove returns
// true, then the directories left empty. A missing root is not an error.
func sweep(root string, remove func(path string, d fs.DirEntry) bool) (int, error) {
	n := 0
	var dirs []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) && path == root {
				return filepath.SkipDir
			}
			return err
		}
		if d.IsDir() {
			if path != root {
				dirs = append(dirs, path)
			}
			return nil
		}
		if remove(path, d) {
			if err := os.RemoveAll(path); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- {
		os.Remove(dirs[i]) // fails, harmlessly, unless empty
	}
	return n, err
}
