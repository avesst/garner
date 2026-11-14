// Package publish renders snapshots and switches the served tree to them
// (design section 9).
package publish

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/avesst/garner/internal/blob"
	"github.com/avesst/garner/internal/config"
	"github.com/avesst/garner/internal/format"
	"github.com/avesst/garner/internal/fsutil"
	"github.com/avesst/garner/internal/id"
	"github.com/avesst/garner/internal/signing"
	"github.com/avesst/garner/internal/store"
)

// Publisher writes snapshots below Root (storage.path).
type Publisher struct {
	Store   *store.Store
	Blobs   *blob.Store
	Formats *format.Registry
	Config  func() *config.Config
	Signer  func() signing.Signer
	// ConfigCommit returns the git commit of the configuration file, or "".
	ConfigCommit func() string
	Root         string
	Log          *slog.Logger

	// mu allows one publish at a time. With multiple nodes this becomes a
	// database lock held by the leader (section 18).
	mu sync.Mutex
}

func (p *Publisher) snapshotsDir() string { return filepath.Join(p.Root, "snapshots") }
func (p *Publisher) publicDir() string    { return filepath.Join(p.Root, "public") }
func (p *Publisher) poolDir(f string) string {
	return filepath.Join(p.Root, "pool", f)
}

// Publish renders the active packages into a new snapshot and makes it
// current.
func (p *Publisher) Publish(ctx context.Context) (*store.Snapshot, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cfg := p.Config()
	active, err := p.Store.Placed(ctx, store.PackageFilter{States: []string{store.Active}})
	if err != nil {
		return nil, err
	}
	keep, drop := p.retain(cfg, active)
	if len(drop) > 0 {
		err := p.Store.Tx(ctx, func(q *store.Queries) error {
			for _, d := range drop {
				if _, err := q.SetState(ctx, d.ID, d.Distribution, store.Removed); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		for _, d := range drop {
			p.Log.Info("retention removed package", "format", d.Format, "name", d.Name,
				"version", d.Version, "arch", d.Arch, "distribution", d.Distribution)
		}
	}
	return p.publish(ctx, cfg, keep)
}

// Rollback publishes a new snapshot with the package set of snapshot id.
// The package placements are changed to match, so that the next publish
// keeps the rolled-back state: packages in the set become active again and
// other active packages are removed.
func (p *Publisher) Rollback(ctx context.Context, snapshotID string) (*store.Snapshot, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cfg := p.Config()
	ok, err := p.Store.SnapshotExists(ctx, snapshotID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("snapshot %s: %w", snapshotID, store.ErrNotFound)
	}
	entries, err := p.Store.SnapshotEntries(ctx, snapshotID)
	if err != nil {
		return nil, err
	}
	want := map[store.SnapshotEntry]bool{}
	for _, e := range entries {
		want[e] = true
	}
	all, err := p.Store.Placed(ctx, store.PackageFilter{})
	if err != nil {
		return nil, err
	}
	var pkgs []*format.Package
	err = p.Store.Tx(ctx, func(q *store.Queries) error {
		for _, pk := range all {
			in := want[store.SnapshotEntry{PackageID: pk.ID, Distribution: pk.Distribution}]
			switch {
			case in:
				pkgs = append(pkgs, pk)
				if pk.State != store.Active {
					if _, err := q.SetState(ctx, pk.ID, pk.Distribution, store.Active); err != nil {
						return err
					}
				}
			case pk.State == store.Active:
				if _, err := q.SetState(ctx, pk.ID, pk.Distribution, store.Removed); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	p.Log.Info("rolling back", "to", snapshotID, "packages", len(pkgs))
	return p.publish(ctx, cfg, pkgs)
}

// retain applies package retention: per format, name, distribution and
// architecture, keep the newest N versions plus pinned packages (section
// 9). N is the delivering source's retention, or defaults.retention; zero
// or less keeps everything.
func (p *Publisher) retain(cfg *config.Config, pkgs []*format.Package) (keep, drop []*format.Package) {
	type key struct{ format, name, dist, arch string }
	groups := map[key][]*format.Package{}
	var order []key
	for _, pk := range pkgs {
		k := key{pk.Format, pk.Name, pk.Distribution, pk.Arch}
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], pk)
	}
	for _, k := range order {
		g := groups[k]
		fm, ok := p.Formats.Get(k.format)
		if !ok {
			drop = append(drop, g...) // a format no longer enabled
			continue
		}
		slices.SortStableFunc(g, func(a, b *format.Package) int { return fm.Compare(b, a) })
		n := 0
		for _, pk := range g {
			if r := retentionFor(cfg, pk); r > n {
				n = r
			}
		}
		for i, pk := range g {
			if n <= 0 || i < n || pk.Pinned {
				keep = append(keep, pk)
			} else {
				drop = append(drop, pk)
			}
		}
	}
	return keep, drop
}

func retentionFor(cfg *config.Config, pk *format.Package) int {
	if pk.Origin == "source" {
		name, _, _ := strings.Cut(pk.OriginRef, "/")
		for _, s := range cfg.Sources {
			if s.Name == name {
				return cfg.Settings(s).Retention
			}
		}
	}
	return cfg.Defaults.Retention
}

func (p *Publisher) publish(ctx context.Context, cfg *config.Config, pkgs []*format.Package) (*store.Snapshot, error) {
	start := time.Now()
	signer := p.Signer()
	snap := &store.Snapshot{ID: id.New(), CreatedAt: store.Now(), ConfigHash: cfg.Hash}
	final := filepath.Join(p.snapshotsDir(), snap.ID)
	tmp := final + ".tmp"
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(tmp)
		}
	}()

	byFormat := map[string][]*format.Package{}
	for _, pk := range pkgs {
		byFormat[pk.Format] = append(byFormat[pk.Format], pk)
	}
	history, err := p.history(ctx, cfg)
	if err != nil {
		return nil, err
	}
	for _, fm := range p.Formats.All() {
		fp := byFormat[fm.Name()]
		if err := p.linkPool(fm, fp); err != nil {
			return nil, err
		}
		var hist []string
		for _, h := range history {
			hist = append(hist, filepath.Join(p.snapshotsDir(), h, fm.Name()))
		}
		err := fm.Render(ctx, &format.RenderRequest{
			Dir: filepath.Join(tmp, fm.Name()), Packages: fp, Config: cfg, Signer: signer,
			PoolDir: p.poolDir(fm.Name()), History: hist,
		})
		if err != nil {
			return nil, fmt.Errorf("render %s: %w", fm.Name(), err)
		}
	}
	if err := p.writeRootFiles(tmp, cfg, signer, snap, pkgs); err != nil {
		return nil, err
	}

	if err := fsutil.SyncTree(tmp); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, final); err != nil {
		return nil, err
	}
	ok = true
	if err := fsutil.SyncDir(p.snapshotsDir()); err != nil {
		return nil, err
	}

	entries := make([]store.SnapshotEntry, len(pkgs))
	for i, pk := range pkgs {
		entries[i] = store.SnapshotEntry{PackageID: pk.ID, Distribution: pk.Distribution}
	}
	if err := p.Store.Tx(ctx, func(q *store.Queries) error {
		return q.InsertSnapshot(ctx, snap, entries)
	}); err != nil {
		return nil, err
	}
	if err := p.reconcileLinks(cfg); err != nil {
		return nil, err
	}
	// The one swap point (sections 9 and 18).
	if err := fsutil.Symlink(filepath.Join("..", "snapshots", snap.ID), filepath.Join(p.publicDir(), "current")); err != nil {
		return nil, fmt.Errorf("switch current: %w", err)
	}
	if err := p.Store.Tx(ctx, func(q *store.Queries) error { return q.SetCurrent(ctx, snap.ID) }); err != nil {
		return nil, err
	}
	snap.IsCurrent = true
	p.Log.Info("published", "snapshot", snap.ID, "packages", len(pkgs), "duration", time.Since(start).Round(time.Millisecond))
	return snap, nil
}

// history returns the snapshots whose index files the new snapshot must
// carry: those created within storage.index_history, and always the
// current one (section 9, step 4).
func (p *Publisher) history(ctx context.Context, cfg *config.Config) ([]string, error) {
	snaps, err := p.Store.Snapshots(ctx)
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().Add(-cfg.Storage.IndexHistory)
	var out []string
	for _, s := range snaps {
		t, err := store.ParseTime(s.CreatedAt)
		if err != nil {
			return nil, err
		}
		if !s.IsCurrent && t.Before(cutoff) {
			continue
		}
		if _, err := os.Stat(filepath.Join(p.snapshotsDir(), s.ID)); err != nil {
			continue // already collected
		}
		out = append(out, s.ID)
	}
	return out, nil
}

// linkPool hardlinks each package's served blob to its pool path.
func (p *Publisher) linkPool(fm format.Format, pkgs []*format.Package) error {
	for _, pk := range pkgs {
		src, err := p.Blobs.Path(pk.ServedSHA256())
		if err != nil {
			return err
		}
		dst := filepath.Join(p.poolDir(fm.Name()), filepath.FromSlash(fm.PoolPath(pk)))
		if err := fsutil.LinkOrCopy(src, dst); err != nil {
			return fmt.Errorf("pool: %w", err)
		}
	}
	return nil
}

// rootFiles are served at the root through public/current.
func (p *Publisher) rootFiles(cfg *config.Config) []string {
	files := []string{"index.html", "key.asc", "key.gpg"}
	for _, fm := range p.Formats.All() {
		files = append(files, fm.Client(cfg, publicURL(cfg)).Name)
	}
	return files
}

// reconcileLinks creates the fixed links of public/ and removes links that
// are no longer configured. It never retargets a link (section 4).
func (p *Publisher) reconcileLinks(cfg *config.Config) error {
	pub := p.publicDir()
	want := map[string]string{"snapshots": "../snapshots"}
	for _, f := range p.rootFiles(cfg) {
		want[f] = "current/" + f
	}
	for _, fm := range p.Formats.All() {
		for _, l := range fm.Links(cfg) {
			want[filepath.FromSlash(l.Path)] = l.Target
		}
	}
	for path, target := range want {
		full := filepath.Join(pub, path)
		if cur, err := os.Readlink(full); err == nil {
			if cur != target {
				return fmt.Errorf("%s points to %s, want %s; refusing to retarget", full, cur, target)
			}
			continue
		}
		if err := fsutil.Symlink(target, full); err != nil {
			return err
		}
	}
	var stale []string
	err := filepath.WalkDir(pub, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(pub, path)
		if d.Type()&fs.ModeSymlink != 0 && rel != "current" && want[rel] == "" {
			stale = append(stale, path)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, s := range stale {
		p.Log.Info("removing link no longer configured", "path", s)
		if err := os.Remove(s); err != nil {
			return err
		}
		removeEmptyParents(filepath.Dir(s), pub)
	}
	return nil
}

func removeEmptyParents(dir, stop string) {
	for dir != stop && strings.HasPrefix(dir, stop) {
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

func publicURL(cfg *config.Config) string {
	return strings.TrimRight(cfg.Server.PublicURL, "/")
}

// Reconcile repairs the database after a crash between the swap of
// public/current and recording it: the link is the truth.
func (p *Publisher) Reconcile(ctx context.Context) error {
	target, err := os.Readlink(filepath.Join(p.publicDir(), "current"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	id := filepath.Base(target)
	cur, err := p.Store.CurrentSnapshot(ctx)
	if err == nil && cur.ID == id {
		return nil
	}
	if ok, err := p.Store.SnapshotExists(ctx, id); err != nil || !ok {
		return fmt.Errorf("public/current points to unknown snapshot %s", id)
	}
	p.Log.Warn("marking the linked snapshot current", "snapshot", id)
	return p.Store.Tx(ctx, func(q *store.Queries) error { return q.SetCurrent(ctx, id) })
}
