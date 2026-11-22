// Package publish renders snapshots and switches the served tree to them
// (design section 9).
package publish

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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
	// Version is garner's version; a new build may render differently.
	Version string
	// Build identifies the executable (BuildID), or is "" if unknown.
	Build string
	Root  string
	Log   *slog.Logger

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

// Stale reports whether a publish would render something other than the
// current snapshot. A start uses it to skip a publish when nothing
// changed: enabled formats, signing keys and garner itself can change
// while garner is stopped, and none of them is in the configuration hash.
func (p *Publisher) Stale(ctx context.Context) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stale(ctx)
}

// stale is Stale with the publish lock held.
func (p *Publisher) stale(ctx context.Context) (bool, error) {
	cur, err := p.Store.CurrentSnapshot(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	cfg := p.Config()
	active, err := p.Store.Placed(ctx, store.PackageFilter{States: []string{store.Active}})
	if err != nil {
		return false, err
	}
	keep, _ := p.retain(cfg, active)
	inputs, err := p.inputs(cfg, p.Signer(), keep)
	if err != nil {
		return false, err
	}
	return inputs != cur.Inputs, nil
}

// inputs digests what a snapshot is rendered from, apart from the time:
// garner's version and build, the configuration and its commit, the
// enabled formats, the signing keys and the package set.
func (p *Publisher) inputs(cfg *config.Config, signer signing.Signer, pkgs []*format.Package) (string, error) {
	h := sha256.New()
	commit := ""
	if p.ConfigCommit != nil {
		commit = p.ConfigCommit()
	}
	fmt.Fprintf(h, "garner snapshot inputs 2\nversion %q\nbuild %q\nconfig %q\ncommit %q\n",
		p.Version, p.Build, cfg.Hash, commit)
	for _, fm := range p.Formats.All() {
		fmt.Fprintf(h, "format %q\n", fm.Name())
	}
	_, keys, err := signer.PublicKeys()
	if err != nil {
		return "", err
	}
	fmt.Fprintf(h, "signer %q\nkeys %x\n", signer.Fingerprint(), sha256.Sum256(keys))
	entries := make([]string, len(pkgs))
	for i, pk := range pkgs {
		entries[i] = fmt.Sprintf("package %q %q\n", pk.ID, pk.Distribution)
	}
	slices.Sort(entries)
	for _, e := range entries {
		io.WriteString(h, e)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Rollback publishes a new snapshot with the package set of snapshot id.
// The package placements are changed to match, so that the next publish
// keeps the rolled-back state: packages in the set become active again and
// other active packages are removed. Packages of a format the snapshot did
// not render, because it was disabled then, keep their state: their
// absence from the set does not mean they were gone.
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
	rendered, err := p.Store.SnapshotFormats(ctx, snapshotID)
	if err != nil {
		return nil, err
	}
	all, err := p.Store.Placed(ctx, store.PackageFilter{})
	if err != nil {
		return nil, err
	}
	// Only enabled formats are published, as in Publish.
	var pkgs []*format.Package
	include := func(pk *format.Package) {
		if _, ok := p.Formats.Get(pk.Format); ok {
			pkgs = append(pkgs, pk)
		}
	}
	err = p.Store.Tx(ctx, func(q *store.Queries) error {
		for _, pk := range all {
			if !rendered[pk.Format] {
				if pk.State == store.Active {
					include(pk)
				}
				continue
			}
			in := want[store.SnapshotEntry{PackageID: pk.ID, Distribution: pk.Distribution}]
			switch {
			case in:
				include(pk)
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
			// A disabled format, perhaps only until its tools are back:
			// not published, and not removed either.
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
	inputs, err := p.inputs(cfg, signer, pkgs)
	if err != nil {
		return nil, err
	}
	snap := &store.Snapshot{ID: id.New(), CreatedAt: store.Now(), ConfigHash: cfg.Hash, Inputs: inputs,
		Formats: p.Formats.Names()}
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
	if err := p.activate(ctx, cfg, snap, pkgs); err != nil {
		// The publish may have failed because ctx was cancelled.
		p.discard(context.WithoutCancel(ctx), snap.ID)
		return nil, err
	}
	snap.IsCurrent = true
	p.Log.Info("published", "snapshot", snap.ID, "packages", len(pkgs), "duration", time.Since(start).Round(time.Millisecond))
	return snap, nil
}

// activate records a rendered snapshot and makes it current.
func (p *Publisher) activate(ctx context.Context, cfg *config.Config, snap *store.Snapshot, pkgs []*format.Package) error {
	if err := fsutil.SyncDir(p.snapshotsDir()); err != nil {
		return err
	}
	entries := make([]store.SnapshotEntry, len(pkgs))
	for i, pk := range pkgs {
		entries[i] = store.SnapshotEntry{PackageID: pk.ID, Distribution: pk.Distribution, ServedSHA256: pk.ServedSHA256()}
	}
	if err := p.Store.Tx(ctx, func(q *store.Queries) error {
		return q.InsertSnapshot(ctx, snap, entries)
	}); err != nil {
		return err
	}
	if err := p.reconcileLinks(cfg); err != nil {
		return err
	}
	// The one swap point (sections 9 and 18).
	if err := fsutil.Symlink(filepath.Join("..", "snapshots", snap.ID), filepath.Join(p.publicDir(), "current")); err != nil {
		return fmt.Errorf("switch current: %w", err)
	}
	return p.Store.Tx(ctx, func(q *store.Queries) error { return q.SetCurrent(ctx, snap.ID) })
}

// discard removes the row and directory of a snapshot whose publish
// failed, so retries do not fill snapshot_retention with snapshots that
// were never served. One public/current already points to stays: the
// swap happened, and Reconcile marks it current at the next start.
func (p *Publisher) discard(ctx context.Context, id string) {
	target, err := os.Readlink(filepath.Join(p.publicDir(), "current"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) || filepath.Base(target) == id {
		return
	}
	if err := p.Store.DeleteSnapshot(ctx, id); err != nil {
		p.Log.Error("cannot remove the snapshot of a failed publish", "snapshot", id, "err", err)
		return
	}
	if err := os.RemoveAll(filepath.Join(p.snapshotsDir(), id)); err != nil {
		p.Log.Error("cannot remove the snapshot of a failed publish", "snapshot", id, "err", err)
	}
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

// linkPool hardlinks each package's served blob to its pool path. The new
// entries are fsynced before it returns: indexes that list them become
// current with the swap, and nothing would recreate a lost link.
func (p *Publisher) linkPool(fm format.Format, pkgs []*format.Package) error {
	// Created even without packages: the pool links in public/ and in the
	// snapshot point here and must resolve.
	if err := os.MkdirAll(p.poolDir(fm.Name()), 0o755); err != nil {
		return fmt.Errorf("pool: %w", err)
	}
	var dirs fsutil.DirSync
	for _, pk := range pkgs {
		src, err := p.Blobs.Path(pk.ServedSHA256())
		if err != nil {
			return err
		}
		rel := filepath.FromSlash(fm.PoolPath(pk))
		if !filepath.IsLocal(rel) {
			return fmt.Errorf("pool: path %q of %s leaves the pool directory", rel, pk.Name)
		}
		dst := filepath.Join(p.poolDir(fm.Name()), rel)
		if err := dirs.LinkOrCopy(src, dst); err != nil {
			return fmt.Errorf("pool: %w", err)
		}
	}
	if err := dirs.Sync(); err != nil {
		return fmt.Errorf("pool: %w", err)
	}
	return nil
}

// rootFiles are served at the root through public/current.
func (p *Publisher) rootFiles(cfg *config.Config) []string {
	files := []string{"index.html", "key.asc", "key.gpg"}
	for _, fm := range p.Formats.All() {
		// Variants are linked once, as their top directory (ClientsDir).
		for _, cf := range fm.Clients(cfg, publicURL(cfg)) {
			if top, _, _ := strings.Cut(cf.Path, "/"); !slices.Contains(files, top) {
				files = append(files, top)
			}
		}
	}
	return files
}

// reconcileLinks creates the fixed links of public/ and removes links that
// are no longer configured. It never retargets a link (section 4).
func (p *Publisher) reconcileLinks(cfg *config.Config) error {
	pub := p.publicDir()
	want := p.fixedLinks(cfg)
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

// fixedLinks maps each fixed link of public/, relative to it, to its target.
func (p *Publisher) fixedLinks(cfg *config.Config) map[string]string {
	want := map[string]string{"snapshots": "../snapshots"}
	for _, f := range p.rootFiles(cfg) {
		want[f] = "current/" + f
	}
	for _, fm := range p.Formats.All() {
		for _, l := range fm.Links(cfg) {
			want[filepath.FromSlash(l.Path)] = l.Target
		}
	}
	return want
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
