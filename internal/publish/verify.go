package publish

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/avesst/garner/internal/format"
	"github.com/avesst/garner/internal/signing"
	"github.com/avesst/garner/internal/store"
)

// VerifyReport is the result of Verify.
type VerifyReport struct {
	Snapshot string `json:"snapshot"`
	// SnapshotConfigHash is the configuration the snapshot was rendered
	// with; ConfigHash is the one garner has loaded.
	SnapshotConfigHash string `json:"snapshot_config_hash"`
	ConfigHash         string `json:"config_hash"`
	// PublishPending is true when a publish would render something other
	// than the current snapshot, such as after a reload or an upload.
	PublishPending bool             `json:"publish_pending"`
	Packages       int              `json:"packages"`
	Problems       []format.Problem `json:"problems"`
	Errors         int              `json:"errors"`
	Warnings       int              `json:"warnings"`
}

// Verify checks that the served tree is consistent and matches the
// database: the current link and snapshot rows, the fixed links, the
// published keys, every served package's pool file and blob, and each
// format's metadata, read the way clients read it. It holds the publish
// lock, so no publish or GC changes the tree while it reads. It changes
// nothing.
func (p *Publisher) Verify(ctx context.Context) (*VerifyReport, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cfg := p.Config()
	rep := &VerifyReport{ConfigHash: cfg.Hash, Problems: []format.Problem{}}
	add := func(pr format.Problem) {
		if rel, err := filepath.Rel(p.Root, pr.Path); err == nil && pr.Path != "" && !strings.HasPrefix(rel, "..") {
			pr.Path = rel
		}
		rep.Problems = append(rep.Problems, pr)
		if pr.Severity == format.SeverityError {
			rep.Errors++
		} else {
			rep.Warnings++
		}
	}

	cur, err := p.Store.CurrentSnapshot(ctx)
	if errors.Is(err, store.ErrNotFound) {
		add(format.Errorf("snapshot", "", "no snapshot has been published"))
		return rep, nil
	}
	if err != nil {
		return nil, err
	}
	rep.Snapshot, rep.SnapshotConfigHash = cur.ID, cur.ConfigHash
	snapDir := filepath.Join(p.snapshotsDir(), cur.ID)

	currentLink := filepath.Join(p.publicDir(), "current")
	if target, err := os.Readlink(currentLink); err != nil {
		add(format.Errorf("current", currentLink, "%v", err))
	} else if target != filepath.Join("..", "snapshots", cur.ID) {
		add(format.Errorf("current", currentLink, "points to %s, but the database marks %s current", target, cur.ID))
	}
	if fi, err := os.Stat(snapDir); err != nil || !fi.IsDir() {
		add(format.Errorf("current", snapDir, "the current snapshot's directory is missing"))
		return rep, nil
	}

	pending, err := p.stale(ctx)
	if err != nil {
		return nil, err
	}
	rep.PublishPending = pending
	if pending {
		add(format.Warnf("publish-pending", "", "the current snapshot does not reflect the loaded configuration, keys or packages yet"))
	}

	snaps, err := p.Store.Snapshots(ctx)
	if err != nil {
		return nil, err
	}
	previous := p.verifySnapshotDirs(cfg.Storage.GCGrace, snaps, add)

	keys := p.verifyKeys(snapDir, pending, add)

	pkgs, err := p.Store.SnapshotPackages(ctx, cur.ID)
	if err != nil {
		return nil, err
	}
	rep.Packages = len(pkgs)
	owned, err := p.Store.PackageBlobs(ctx)
	if err != nil {
		return nil, err
	}
	snapTags, err := p.Store.SnapshotTags(ctx, cur.ID)
	if err != nil {
		return nil, err
	}
	tagsByFormat := map[string][]format.Tag{}
	for _, t := range snapTags {
		tagsByFormat[t.Format] = append(tagsByFormat[t.Format], t)
	}
	byFormat := map[string][]*format.Package{}
	for _, pk := range pkgs {
		pk.Blobs = owned[pk.ID]
		byFormat[pk.Format] = append(byFormat[pk.Format], pk)
		p.verifyPool(pk, add)
		if err := p.verifyOwnedBlobs(ctx, pk, add); err != nil {
			return nil, err
		}
	}

	links := p.fixedLinks(cfg)
	p.verifyLinks(links, add)
	if err := p.verifyGarbage(ctx, cfg.Storage.GCGrace, add); err != nil {
		return nil, err
	}

	if keys != nil {
		for _, fm := range p.Formats.All() {
			req := &format.VerifyRequest{
				Dir: filepath.Join(snapDir, fm.Name()), Packages: byFormat[fm.Name()], Config: cfg, Keys: keys,
				Tags: tagsByFormat[fm.Name()],
			}
			if previous != "" {
				req.Previous = filepath.Join(p.snapshotsDir(), previous, fm.Name())
			}
			for _, pr := range fm.Verify(ctx, req) {
				add(pr)
			}
		}
	}
	for name, n := range byFormat {
		if _, ok := p.Formats.Get(name); !ok {
			add(format.Warnf("format", "", "the snapshot serves %d %s packages, but %s is not enabled", len(n), name, name))
		}
	}
	return rep, nil
}

// verifySnapshotDirs checks that snapshot rows and directories match and
// returns the snapshot that was current before the current one, if its
// directory exists.
func (p *Publisher) verifySnapshotDirs(grace time.Duration, snaps []store.Snapshot, add func(format.Problem)) string {
	recorded := map[string]bool{}
	previous, replaced := "", ""
	for _, s := range snaps {
		recorded[s.ID] = true
		dir := filepath.Join(p.snapshotsDir(), s.ID)
		if _, err := os.Stat(dir); err != nil {
			add(format.Errorf("snapshot", dir, "snapshot %s is recorded, but its directory is missing", s.ID))
			continue
		}
		if !s.IsCurrent && s.ReplacedAt > replaced {
			previous, replaced = s.ID, s.ReplacedAt
		}
	}
	entries, err := os.ReadDir(p.snapshotsDir())
	if err != nil {
		add(format.Errorf("snapshot", p.snapshotsDir(), "%v", err))
		return previous
	}
	for _, e := range entries {
		if recorded[e.Name()] {
			continue
		}
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < grace {
			continue // a publish in progress, or GC has not had its chance
		}
		add(format.Warnf("garbage", filepath.Join(p.snapshotsDir(), e.Name()),
			"snapshot directory without a database row, left by a failed publish; GC removes it"))
	}
	return previous
}

// verifyKeys reads the snapshot's key files, checks they hold the same
// keys, and returns them for checking signatures. Keys that differ from
// the signing keys are an error unless a publish is pending, which will
// publish the new ones.
func (p *Publisher) verifyKeys(snapDir string, pending bool, add func(format.Problem)) *signing.PublicKeyring {
	read := func(name string) *signing.PublicKeyring {
		path := filepath.Join(snapDir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			add(format.Errorf("keys", path, "%v", err))
			return nil
		}
		k, err := signing.ReadPublicKeys(data)
		if err != nil {
			add(format.Errorf("keys", path, "%v", err))
			return nil
		}
		return k
	}
	asc, gpg := read("key.asc"), read("key.gpg")
	if asc == nil || gpg == nil {
		return nil
	}
	if !slices.Equal(asc.Fingerprints(), gpg.Fingerprints()) {
		add(format.Errorf("keys", filepath.Join(snapDir, "key.gpg"), "holds other keys than key.asc"))
	}
	if !pending {
		armored, _, err := p.Signer().PublicKeys()
		if err == nil {
			if signer, err := signing.ReadPublicKeys(armored); err == nil &&
				!slices.Equal(signer.Fingerprints(), gpg.Fingerprints()) {
				add(format.Errorf("keys", filepath.Join(snapDir, "key.gpg"), "the published keys %v are not the configured keys %v",
					gpg.Fingerprints(), signer.Fingerprints()))
			}
		}
	}
	return gpg
}

// verifyPool checks a served package's blob and pool file. Sizes are
// compared with the blob's recorded size; contents are not re-hashed.
func (p *Publisher) verifyPool(pk *format.Package, add func(format.Problem)) {
	fm, ok := p.Formats.Get(pk.Format)
	if !ok {
		return
	}
	served := pk.ServedSHA256()
	desc := fmt.Sprintf("%s %s %s (%s)", pk.Format, pk.Name, pk.Version, pk.Distribution)
	if pk.ServedSize < 0 {
		add(format.Errorf("blob", "", "%s: blob %s has no database row", desc, served))
		return
	}
	blobPath, err := p.Blobs.Path(served)
	if err != nil {
		add(format.Errorf("blob", "", "%s: %v", desc, err))
		return
	}
	bfi, err := os.Stat(blobPath)
	if err != nil {
		add(format.Errorf("blob", blobPath, "%s: %v", desc, err))
	} else if bfi.Size() != pk.ServedSize {
		add(format.Errorf("blob", blobPath, "%s: has %d bytes, recorded %d", desc, bfi.Size(), pk.ServedSize))
	}
	poolPath := filepath.Join(p.poolDir(fm.Name()), filepath.FromSlash(fm.PoolPath(pk)))
	pfi, err := os.Stat(poolPath)
	if err != nil {
		add(format.Errorf("pool", poolPath, "%s: %v", desc, err))
		return
	}
	// A copy, where hardlinks are not possible, is fine if its size matches.
	if pfi.Size() != pk.ServedSize {
		add(format.Errorf("pool", poolPath, "%s: has %d bytes, the blob %d", desc, pfi.Size(), pk.ServedSize))
	}
}

// verifyOwnedBlobs checks the further blobs of a package (section 19):
// each has a row, a file of the recorded size and its pool file.
func (p *Publisher) verifyOwnedBlobs(ctx context.Context, pk *format.Package, add func(format.Problem)) error {
	fm, ok := p.Formats.Get(pk.Format)
	if !ok || len(pk.Blobs) == 0 {
		return nil
	}
	bp, ok := fm.(format.BlobPooler)
	desc := fmt.Sprintf("%s %s %s", pk.Format, pk.Name, pk.Version)
	if !ok {
		add(format.Errorf("blob", "", "%s: owns %d further blobs, but %s does not pool them", desc, len(pk.Blobs), fm.Name()))
		return nil
	}
	for _, sha := range pk.Blobs {
		size, err := p.Store.BlobSize(ctx, sha)
		if errors.Is(err, store.ErrNotFound) {
			add(format.Errorf("blob", "", "%s: blob %s has no database row", desc, sha))
			continue
		}
		if err != nil {
			return err
		}
		blobPath, err := p.Blobs.Path(sha)
		if err != nil {
			add(format.Errorf("blob", "", "%s: %v", desc, err))
			continue
		}
		for _, f := range []struct{ check, path string }{
			{"blob", blobPath},
			{"pool", filepath.Join(p.poolDir(fm.Name()), filepath.FromSlash(bp.BlobPoolPath(sha)))},
		} {
			fi, err := os.Stat(f.path)
			switch {
			case err != nil:
				add(format.Errorf(f.check, f.path, "%s: %v", desc, err))
			case fi.Size() != size:
				add(format.Errorf(f.check, f.path, "%s: has %d bytes, recorded %d", desc, fi.Size(), size))
			}
		}
	}
	return nil
}

// verifyLinks checks public/'s fixed links. A link no longer configured is
// a warning: the next publish removes it.
func (p *Publisher) verifyLinks(want map[string]string, add func(format.Problem)) {
	pub := p.publicDir()
	for rel, target := range want {
		full := filepath.Join(pub, rel)
		got, err := os.Readlink(full)
		if err != nil {
			add(format.Errorf("link", full, "%v", err))
			continue
		}
		if got != target {
			add(format.Errorf("link", full, "points to %s, want %s", got, target))
			continue
		}
		if _, err := os.Stat(full); err != nil {
			add(format.Errorf("link", full, "does not resolve: %v", err))
		}
	}
	filepath.WalkDir(pub, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(pub, path)
		if d.Type()&fs.ModeSymlink != 0 && rel != "current" && want[rel] == "" {
			add(format.Warnf("link", path, "link is no longer configured; the next publish removes it"))
		}
		return nil
	})
}

// verifyGarbage reports what GC would remove: pool files and blob files no
// retained snapshot or live package references, and temp files, once
// older than gc_grace. Younger ones belong to uploads and publishes in
// progress.
func (p *Publisher) verifyGarbage(ctx context.Context, grace time.Duration, add func(format.Problem)) error {
	live, err := p.Store.ReferencedBlobs(ctx)
	if err != nil {
		return err
	}
	known, err := p.Store.Blobs(ctx)
	if err != nil {
		return err
	}
	old := func(d fs.DirEntry) bool {
		info, err := d.Info()
		return err == nil && time.Since(info.ModTime()) > grace
	}
	count := func(root string, garbage func(path string, d fs.DirEntry) bool) int {
		n := 0
		filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() && garbage(path, d) {
				n++
			}
			return nil
		})
		return n
	}
	for _, fm := range p.Formats.All() {
		root := p.poolDir(fm.Name())
		garbage := poolGarbage(fm, root, live)
		if n := count(root, func(path string, d fs.DirEntry) bool {
			return garbage(path)
		}); n > 0 {
			add(format.Warnf("garbage", root, "%d pool files no snapshot or package references; GC removes them", n))
		}
	}
	blobs := filepath.Join(p.Root, "blobs", "sha256")
	if n := count(blobs, func(path string, d fs.DirEntry) bool {
		_, recorded := known[d.Name()]
		return !recorded && !live[d.Name()] && old(d)
	}); n > 0 {
		add(format.Warnf("garbage", blobs, "%d blob files without a database row; GC removes them", n))
	}
	for _, dir := range []string{filepath.Join(p.Root, "blobs", "tmp"), filepath.Join(p.Root, "tmp")} {
		if _, n, err := staleTemp(dir, time.Now().Add(-grace)); err != nil {
			return err
		} else if n > 0 {
			add(format.Warnf("garbage", dir, "%d temporary files older than gc_grace, left by failed uploads; GC removes them", n))
		}
	}
	return nil
}
