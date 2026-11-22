package oci

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/avesst/garner/internal/blob"
	"github.com/avesst/garner/internal/config"
	"github.com/avesst/garner/internal/format"
	"github.com/avesst/garner/internal/glob"
	"github.com/avesst/garner/internal/ingest"
	"github.com/avesst/garner/internal/server"
	"github.com/avesst/garner/internal/store"
	gsync "github.com/avesst/garner/internal/sync"
)

// Syncer copies the matching tags of a registry source's image (section
// 19). source_tags holds the upstream digest each local tag was last
// applied from, so a tag moves only when upstream moves it.
// source_tag_candidates holds the digests seen under a tag with their
// first sighting, which min_release_age runs from. A processed digest is
// recorded in seen_releases with the image as repo.
type Syncer struct {
	Store    *store.Store
	Blobs    *blob.Store
	Ingester *ingest.Ingester
	// NewClient builds registry clients; tests replace it.
	NewClient func(config.Registry) (*Client, error)
	// Now is the clock for min_release_age; tests replace it.
	Now func() time.Time

	// clients are shared by the sources of a registry, so max_concurrent
	// holds across them; one is rebuilt when its settings change.
	mu      sync.Mutex
	clients map[string]cachedClient
}

type cachedClient struct {
	cfg    config.Registry
	client *Client
}

// Reset drops the registry clients, so the next sync builds them again
// and reads rotated tokens and CA files, as forge clients are on reload.
func (s *Syncer) Reset() {
	s.mu.Lock()
	s.clients = nil
	s.mu.Unlock()
}

// client returns the shared client of a registry.
func (s *Syncer) client(r config.Registry) (*Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.clients[r.Name]; ok && reflect.DeepEqual(c.cfg, r) {
		return c.client, nil
	}
	newClient := s.NewClient
	if newClient == nil {
		newClient = NewClient
	}
	c, err := newClient(r)
	if err != nil {
		return nil, err
	}
	if s.clients == nil {
		s.clients = map[string]cachedClient{}
	}
	s.clients[r.Name] = cachedClient{cfg: r, client: c}
	return c, nil
}

var _ gsync.ImageSyncer = (*Syncer)(nil)

func (s *Syncer) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// SyncImages syncs one registry source.
func (s *Syncer) SyncImages(ctx context.Context, src config.Source, cfg *config.Config, log *slog.Logger) (gsync.ImageResult, error) {
	var res gsync.ImageResult
	idx := slices.IndexFunc(cfg.Registries, func(r config.Registry) bool { return r.Name == src.Registry })
	if idx < 0 {
		return res, fmt.Errorf("unknown registry %q", src.Registry)
	}
	c, err := s.client(cfg.Registries[idx])
	if err != nil {
		return res, err
	}
	tags, err := c.Tags(ctx, src.Image)
	if err != nil {
		return res, err
	}
	settings := cfg.Settings(src)
	var errs []error
	for _, tag := range tags {
		if !ValidTag(tag) || !slices.ContainsFunc(src.Tags, func(g string) bool { return glob.Match(g, tag) }) {
			continue
		}
		added, deferred, err := s.syncTag(ctx, c, src, settings, cfg, tag, log)
		if errors.Is(err, context.Canceled) {
			return res, err
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s:%s: %w", src.Image, tag, err))
			continue
		}
		if added {
			res.Added++
		}
		if deferred != nil {
			res.Deferred = append(res.Deferred, *deferred)
		}
	}
	return res, errors.Join(errs...)
}

// syncTag brings one tag up to date. A tag moves only when upstream moves
// it away from the digest it was last applied from (source_tags), so a
// rollback or a deleted tag holds until then. With min_release_age, the
// newest digest seen under the tag that is old enough is applied, as a
// forge source applies the newest release that is old enough; the rest
// wait as candidates.
func (s *Syncer) syncTag(ctx context.Context, c *Client, src config.Source, settings config.Settings,
	cfg *config.Config, tag string, log *slog.Logger) (bool, *server.DeferredRelease, error) {
	key := store.TagKey{Source: src.Name, Repo: src.Image, Name: src.LocalName(), Tag: tag}
	upstream, err := c.Resolve(ctx, src.Image, tag)
	if err != nil {
		return false, nil, err
	}
	applied, err := s.Store.SourceTag(ctx, key)
	if err != nil {
		return false, nil, err
	}
	target := upstream
	var deferred *server.DeferredRelease
	if settings.MinReleaseAge > 0 {
		var first time.Time
		target, first, deferred, err = s.ageGate(ctx, key, upstream, applied, settings.MinReleaseAge)
		if err != nil || target == "" {
			if deferred != nil {
				log.Debug("image waits for min_release_age", "image", src.Image, "tag", tag,
					"digest", deferred.ID, "eligible_at", deferred.EligibleAt)
			}
			return false, deferred, err
		}
		defer func() {
			// Older candidates are never applied once a newer one was.
			if applied, _ := s.Store.SourceTag(ctx, key); applied == target {
				s.Store.DropTagCandidates(ctx, key, first, time.Time{})
			}
		}()
	}
	if target == applied {
		return false, deferred, nil
	}
	added, err := s.apply(ctx, c, src, key, settings, cfg, target, log)
	if err != nil && target != upstream && s.goneUpstream(ctx, c, src.Image, target) {
		// An older candidate upstream has deleted; the next poll picks
		// another. Other failures, such as a CDN error on one layer, keep
		// it for the next poll.
		log.Warn("waiting image digest is gone upstream; dropping it", "image", src.Image, "tag", tag, "digest", target)
		return false, deferred, s.Store.DropTagCandidate(ctx, key, target)
	}
	return added, deferred, err
}

// goneUpstream reports whether the registry answers 404 for the manifest
// itself.
func (s *Syncer) goneUpstream(ctx context.Context, c *Client, image, digest string) bool {
	_, err := c.Resolve(ctx, image, digest)
	var se *StatusError
	return errors.As(err, &se) && se.Status == http.StatusNotFound
}

// ageGate records upstream as a candidate of the tag and returns the
// newest candidate old enough to apply, with its first sighting, or ""
// when none is. deferred describes the newest candidate while it waits.
// Candidates first seen after the digest upstream points at now were
// withdrawn upstream and are dropped; when upstream points at the applied
// digest, every candidate is.
func (s *Syncer) ageGate(ctx context.Context, key store.TagKey, upstream, applied string,
	minAge time.Duration) (string, time.Time, *server.DeferredRelease, error) {
	now := s.now()
	if upstream == applied {
		return "", time.Time{}, nil, s.Store.DropTagCandidates(ctx, key, time.Time{}, time.Unix(0, 0))
	}
	if err := s.Store.AddTagCandidate(ctx, key, upstream, now); err != nil {
		return "", time.Time{}, nil, err
	}
	cands, err := s.Store.TagCandidates(ctx, key)
	if err != nil {
		return "", time.Time{}, nil, err
	}
	for i, c := range cands {
		if c.Digest == upstream {
			if i > 0 {
				if err := s.Store.DropTagCandidates(ctx, key, time.Time{}, c.FirstSeen); err != nil {
					return "", time.Time{}, nil, err
				}
			}
			cands = cands[i:]
			break
		}
	}
	override, overridden, err := s.Store.AgeOverride(ctx, key.Source, key.Repo, key.Tag)
	if err != nil {
		return "", time.Time{}, nil, err
	}
	var deferred *server.DeferredRelease
	for i, c := range cands {
		eligible := c.FirstSeen.Add(minAge)
		if !now.Before(eligible) || overridden && !override.Before(c.FirstSeen) {
			return c.Digest, c.FirstSeen, deferred, nil
		}
		if i == 0 {
			deferred = &server.DeferredRelease{ID: c.Digest, Repo: key.Repo, Tag: key.Tag,
				PublishedAt: store.FormatTime(c.FirstSeen), EligibleAt: store.FormatTime(eligible)}
		}
	}
	return "", time.Time{}, deferred, nil
}

// apply points the tag at digest, fetching it only if garner has not
// processed it before. It reports whether a new image was stored.
func (s *Syncer) apply(ctx context.Context, c *Client, src config.Source, key store.TagKey, settings config.Settings,
	cfg *config.Config, digest string, log *slog.Logger) (bool, error) {
	name, tag := key.Name, key.Tag
	setApplied := func() error { return s.Store.SetSourceTag(ctx, key, digest) }
	if cur, err := s.Store.GetTag(ctx, Format{}.Name(), name, tag); err == nil {
		if p, err := s.Store.PackageByID(ctx, cur.PackageID); err == nil && p.Version == digest {
			return false, setApplied()
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return false, err
	}
	seen, err := s.Store.SeenRelease(ctx, src.Name, src.Image, digest)
	if err != nil {
		return false, err
	}
	var placed []*format.Package
	if seen {
		// Seen under another local name (as: changed) counts as new here.
		if placed, err = s.Store.Placed(ctx, store.PackageFilter{Format: Format{}.Name(), Name: name,
			Version: digest}); err != nil {
			return false, err
		}
		seen = len(placed) > 0
	}
	if seen {
		var active, held *format.Package
		for _, p := range placed {
			switch p.State {
			case store.Active:
				active = p
			case store.Held:
				held = p
			}
		}
		switch {
		case active == nil && held == nil:
			// Removed by retention, a rollback or a delete: it stays
			// removed, as a processed forge release is never ingested
			// twice.
			log.Debug("image digest was removed; not syncing it again", "image", src.Image, "tag", tag, "digest", digest)
			return false, setApplied()
		case active == nil:
			// Held: nothing is fetched again, and its blobs are not
			// staged, until it is approved.
			return false, nil
		}
		r, err := s.recordStored(ctx, active, tag, src)
		if err != nil {
			return false, err
		}
		if r.State == store.Active && r.Conflict == "" {
			return false, setApplied()
		}
		return false, nil
	}

	tree, err := s.fetchTree(ctx, c, src.Image, digest, cfg)
	if err != nil {
		return false, err
	}
	r, err := s.Ingester.Record(ctx, delivery(tree, name, []string{tag}, s.request(src, tag, settings)))
	if err != nil {
		var ie *ingest.Error
		if errors.As(err, &ie) {
			// Recorded as processed: the same digest fails the same way
			// on every poll.
			log.Warn("image not stored", "image", src.Image, "tag", tag, "digest", digest, "err", err)
			if merr := s.Store.MarkSeen(ctx, src.Name, src.Image, tag, digest, s.now()); merr != nil {
				return false, merr
			}
		}
		return false, err
	}
	if err := s.Store.MarkSeen(ctx, src.Name, src.Image, tag, digest, s.now()); err != nil {
		return false, err
	}
	if r.State == store.Held {
		// The package owns its blobs now; their stage would let pushes use
		// a held image.
		if err := s.Store.UnstageBlobs(ctx, append([]string{r.Package.BlobSHA256}, tree.Blobs...)); err != nil {
			return false, err
		}
	}
	if r.State == store.Active && r.Conflict == "" {
		if err := setApplied(); err != nil {
			return false, err
		}
	}
	log.Info("image synced", "image", src.Image, "tag", tag, "digest", digest, "name", name,
		"state", r.State, "existing", r.Existing)
	return !r.Existing, nil
}

func (s *Syncer) request(src config.Source, tag string, settings config.Settings) ingest.Request {
	return ingest.Request{
		Owner: ingest.SourceOwner(src.Name), Origin: "source",
		OriginRef: src.Name + "/" + src.Image + ":" + tag, VerifyStatus: "verified",
		Held: settings.Approval == "manual", HoldOnOwnerConflict: true,
	}
}

// recordStored points the tag at an image garner already has, without
// fetching anything.
func (s *Syncer) recordStored(ctx context.Context, p *format.Package, tag string, src config.Source) (*ingest.Result, error) {
	f, err := s.Blobs.Open(p.BlobSHA256)
	if err != nil {
		return nil, err
	}
	content, err := io.ReadAll(io.LimitReader(f, MaxManifestSize+1))
	f.Close()
	if err != nil {
		return nil, err
	}
	blobs, err := s.Store.PackageBlobsOf(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	return s.Ingester.Record(ctx, ingest.Delivery{
		Format:  Format{},
		Parsed:  &format.Parsed{Name: p.Name, Version: p.Version, Metadata: p.Metadata},
		Content: content,
		Blobs:   blobs,
		Tags:    []string{tag},
		Request: s.request(src, tag, config.Settings{}),
	})
}

// fetchTree downloads a manifest and everything below it that is not
// stored yet. Blobs are staged until Record makes them the package's.
func (s *Syncer) fetchTree(ctx context.Context, c *Client, image, digest string, cfg *config.Config) (*Tree, error) {
	body, ct, err := c.Manifest(ctx, image, digest)
	if err != nil {
		return nil, err
	}
	expiry := 24 * time.Hour
	if cfg.OCI != nil {
		expiry = cfg.OCI.UploadExpiry
	}
	stage := func(b blob.Blob, f io.ReaderAt) error {
		return s.Store.Tx(ctx, func(q *store.Queries) error {
			if err := q.AddBlob(ctx, b.SHA256, b.Size); err != nil {
				return err
			}
			if err := s.Blobs.Ensure(b.SHA256, f, b.Size); err != nil {
				return err
			}
			return q.StageBlob(ctx, b.SHA256, s.now().Add(expiry))
		})
	}
	fetch := func(sha string) ([]byte, error) {
		child, _, err := c.Manifest(ctx, image, "sha256:"+sha)
		if err != nil {
			return nil, err
		}
		b, f, err := s.Blobs.PutOpen(bytes.NewReader(child))
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return child, stage(b, f)
	}
	tree, err := Walk(body, ct, fetch)
	if err != nil {
		return nil, err
	}
	limit := int64(cfg.Storage.MaxPackageSize)
	var missing []string
	for _, sha := range tree.Blobs {
		if _, err := s.Store.BlobSize(ctx, sha); err == nil {
			// Stored already. A blob a package or snapshot references is
			// kept from GC anyway; staging it would make a held package's
			// blob usable by pushes. Others are kept until Record.
			ref, err := s.Store.BlobReferenced(ctx, sha)
			if err != nil {
				return nil, err
			}
			if !ref {
				if err := s.Store.StageBlob(ctx, sha, s.now().Add(expiry)); err != nil {
					return nil, err
				}
			}
			continue
		} else if !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
		// Checked before any download: a too-large blob would otherwise
		// be fetched up to the limit on every poll and then refused.
		if size := tree.Sizes[sha]; size > limit {
			return nil, fmt.Errorf("blob sha256:%s is %d bytes, more than storage.max_package_size (%d)", sha, size, limit)
		}
		missing = append(missing, sha)
	}
	for _, sha := range missing {
		if err := s.download(ctx, c, image, sha, limit, stage); err != nil {
			return nil, err
		}
	}
	return tree, nil
}

func (s *Syncer) download(ctx context.Context, c *Client, image, sha string, limit int64,
	stage func(blob.Blob, io.ReaderAt) error) error {
	rc, err := c.Blob(ctx, image, "sha256:"+sha)
	if err != nil {
		return err
	}
	defer rc.Close()
	lr := &io.LimitedReader{R: rc, N: limit + 1}
	b, f, err := s.Blobs.PutOpen(lr)
	if err != nil {
		return err
	}
	defer f.Close()
	if lr.N <= 0 {
		return fmt.Errorf("blob sha256:%s exceeds storage.max_package_size", sha)
	}
	if b.SHA256 != sha {
		// Left unreferenced; GC removes it.
		return fmt.Errorf("blob sha256:%s downloaded with digest sha256:%s", sha, b.SHA256)
	}
	return stage(b, f)
}
