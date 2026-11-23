package publish

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avesst/garner/internal/format"
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
		// Requests resolved through a snapshot get gc_grace to finish
		// after it stopped being current. One that never was current is
		// left over from a crashed publish.
		since := s.ReplacedAt
		if since == "" {
			since = s.CreatedAt
		}
		replaced, err := store.ParseTime(since)
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
		clock := now
		if p.Now != nil {
			clock = p.Now()
		}
		if _, err := q.DeleteExpiredStaged(ctx, clock); err != nil {
			return err
		}
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
	for _, fm := range p.Formats.All() {
		root := p.poolDir(fm.Name())
		garbage := poolGarbage(fm, root, live)
		n, err := sweep(root, func(path string, d fs.DirEntry) bool {
			return garbage(path)
		})
		if err != nil {
			return nil, err
		}
		res.PoolFiles += n
	}

	for _, sha := range gone {
		removed, err := p.removeBlob(ctx, sha)
		if err != nil {
			return nil, err
		}
		if removed {
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
	var orphans []string
	n, err := sweep(filepath.Join(p.Root, "blobs", "sha256"), func(path string, d fs.DirEntry) bool {
		if _, ok := known[d.Name()]; ok || live[d.Name()] || !old(d) {
			return false
		}
		if _, err := p.Blobs.Path(d.Name()); err != nil {
			return true // not a blob
		}
		orphans = append(orphans, d.Name())
		return false
	})
	if err != nil {
		return nil, err
	}
	res.Blobs += n
	for _, sha := range orphans {
		removed, err := p.removeBlob(ctx, sha)
		if err != nil {
			return nil, err
		}
		if removed {
			res.Blobs++
		}
	}
	for _, dir := range []string{filepath.Join(p.Root, "blobs", "tmp"), filepath.Join(p.Root, "tmp")} {
		n, err := sweepTemp(dir, now.Add(-grace))
		if err != nil {
			return nil, err
		}
		res.TempFiles += n
	}
	entries, err := os.ReadDir(p.snapshotsDir())
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	recorded := map[string]bool{}
	for _, s := range snaps {
		recorded[s.ID] = true
	}
	for _, e := range entries {
		switch {
		case !old(e) || recorded[e.Name()]:
		case strings.HasSuffix(e.Name(), ".tmp"):
			os.RemoveAll(filepath.Join(p.snapshotsDir(), e.Name()))
			res.TempFiles++
		default:
			// Renamed into place, but the publish crashed before
			// recording it.
			if err := os.RemoveAll(filepath.Join(p.snapshotsDir(), e.Name())); err != nil {
				return nil, err
			}
			res.Snapshots = append(res.Snapshots, e.Name())
		}
	}
	for _, c := range p.Collect {
		if err := c(ctx); err != nil {
			return nil, err
		}
	}
	p.Log.Info("gc finished", "snapshots", len(res.Snapshots), "blobs", res.Blobs,
		"pool_files", res.PoolFiles, "temp_files", res.TempFiles)
	return res, nil
}

// removeBlob deletes the row and file of a blob that no package references.
// It reports false, keeping both, when an upload has added a package with
// this content since the blob was found unreferenced. The upload restores
// the file if it was removed first (store.RemoveBlob, store.AddBlob).
func (p *Publisher) removeBlob(ctx context.Context, sha string) (bool, error) {
	path, err := p.Blobs.Path(sha)
	if err != nil {
		return false, err
	}
	removed := false
	err = p.Store.Tx(ctx, func(q *store.Queries) error {
		removed = false
		_, err := q.RemoveBlob(ctx, sha, func() error {
			err := os.Remove(path)
			removed = err == nil
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		})
		return err
	})
	return removed && err == nil, err
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

// sweepTemp removes the stale entries directly below root (staleTemp)
// and returns how many files went with them.
func sweepTemp(root string, cutoff time.Time) (int, error) {
	stale, n, err := staleTemp(root, cutoff)
	if err != nil {
		return 0, err
	}
	for _, path := range stale {
		if err := os.RemoveAll(path); err != nil {
			return 0, err
		}
	}
	return n, nil
}

// staleTemp returns the entries directly below root that nothing has
// written since cutoff, and how many files they hold. A directory is one
// unit, judged by the newest of itself and everything in it: a sync keeps
// earlier files in its directory while it works on later ones, and an
// empty directory may be waiting for its first file. Whoever holds a temp
// directory and may pause longer than gc_grace without writing to it
// touches it (sync.syncRelease). A missing root is not an error.
func staleTemp(root string, cutoff time.Time) ([]string, int, error) {
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	var stale []string
	n := 0
	for _, e := range entries {
		path := filepath.Join(root, e.Name())
		fresh, files := false, 0
		err := filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
			if err == nil {
				var info fs.FileInfo
				if info, err = d.Info(); err == nil {
					if !info.ModTime().Before(cutoff) {
						fresh = true
						return filepath.SkipAll
					}
					if !d.IsDir() {
						files++
					}
				}
			}
			if os.IsNotExist(err) {
				return nil // removed by its owner meanwhile
			}
			return err
		})
		if err != nil {
			return nil, 0, err
		}
		if !fresh {
			stale = append(stale, path)
			n += files
		}
	}
	return stale, n, nil
}

// poolGarbage returns whether a file below a format's pool directory root
// holds no live blob. Pool files of most formats sit in a directory named
// after the first 16 hex digits of their blob (section 4); a BlobPooler's
// are at exactly BlobPoolPath.
func poolGarbage(fm format.Format, root string, live map[string]bool) func(path string) bool {
	if bp, ok := fm.(format.BlobPooler); ok {
		want := map[string]bool{}
		for sha := range live {
			want[filepath.Join(root, filepath.FromSlash(bp.BlobPoolPath(sha)))] = true
		}
		return func(path string) bool { return !want[path] }
	}
	prefixes := map[string]bool{}
	for sha := range live {
		prefixes[sha[:min(16, len(sha))]] = true
	}
	return func(path string) bool { return !prefixes[filepath.Base(filepath.Dir(path))] }
}
