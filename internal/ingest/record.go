package ingest

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/avesst/garner/internal/format"
	"github.com/avesst/garner/internal/id"
	"github.com/avesst/garner/internal/store"
)

// Delivery is a package whose files are stored already, such as an image
// pushed blob by blob or synced from a registry (section 19). Ownership,
// the content check, placement and the publish request are the same as
// for Ingest.
type Delivery struct {
	Format format.Format
	Parsed *format.Parsed
	// Content is the package's own blob, such as a manifest, which Record
	// stores.
	Content []byte
	// Blobs are the further blobs the package owns. Each must be stored
	// and recorded already (store.AddBlob).
	Blobs []string
	// Tags are pointed at the package once it is active.
	Tags []string
	Request
}

// Record stores d's content and records the package, its blobs and tags.
func (in *Ingester) Record(ctx context.Context, d Delivery) (*Result, error) {
	cfg := in.Config()
	fm, parsed := d.Format, d.Parsed
	b, f, err := in.Blobs.PutOpen(bytes.NewReader(d.Content))
	if err != nil {
		return nil, err
	}
	defer f.Close()

	dist := d.Targets[fm.DistributionKey()]
	if dist == "" {
		dist = format.DefaultDistribution(fm.Distributions(cfg))
	}
	ident := store.Identity{Format: fm.Name(), Name: parsed.Name, Epoch: parsed.Epoch,
		Version: parsed.Version, Release: parsed.Release, Arch: parsed.Arch}
	ref := fmt.Sprintf("%s %s@%s", fm.Name(), parsed.Name, parsed.Version)

	res := &Result{Distribution: dist}
	err = in.Store.Tx(ctx, func(q *store.Queries) error {
		res.Existing, res.Conflict, res.State, res.Package = false, "", "", nil
		for _, sha := range d.Blobs {
			if _, err := q.BlobSize(ctx, sha); errors.Is(err, store.ErrNotFound) {
				return invalid("%s refers to blob sha256:%s, which is not stored", ref, sha)
			} else if err != nil {
				return err
			}
		}
		variants, err := q.PlacedVariants(ctx, ident, dist)
		if err != nil {
			return err
		}
		for _, v := range variants {
			if v.BlobSHA256 == b.SHA256 {
				res.Package, res.State, res.Existing = v.Package, v.State, true
			} else if v.DecidesVersion() {
				return conflict("%s is already in %s with different content (sha256 %s)", ref, dist, v.BlobSHA256)
			}
		}

		ownerConflict, claim, err := in.checkOwner(ctx, q, cfg, fm, parsed.Name, d.Owner)
		if err != nil {
			return err
		}
		if res.Existing {
			if ownerConflict != "" {
				if !d.HoldOnOwnerConflict {
					return conflict("%s", ownerConflict)
				}
				res.Conflict = ownerConflict
				return nil
			}
			return in.tag(ctx, q, fm, parsed.Name, res, d.Tags, d.Owner)
		}

		state := store.Active
		if d.Held {
			state = store.Held
		}
		if ownerConflict != "" {
			if !d.HoldOnOwnerConflict {
				return conflict("%s", ownerConflict)
			}
			state = store.Held
			res.Conflict = ownerConflict
			if err := q.AddConflict(ctx, id.New(), "ownership", ref, ownerConflict); err != nil {
				return err
			}
		} else if claim != nil {
			if err := q.SetOwner(ctx, fm.Namespace(), claim.name, d.Owner, claim.source); err != nil {
				return err
			}
		}

		pkg, err := q.FindPackage(ctx, ident, b.SHA256)
		if errors.Is(err, store.ErrNotFound) {
			if err := q.AddBlob(ctx, b.SHA256, b.Size); err != nil {
				return err
			}
			if err := in.Blobs.Ensure(b.SHA256, f, b.Size); err != nil {
				return err
			}
			pkg = &format.Package{
				ID: id.New(), Format: fm.Name(), Name: parsed.Name, Epoch: parsed.Epoch,
				Version: parsed.Version, Release: parsed.Release, Arch: parsed.Arch,
				BlobSHA256: b.SHA256, Metadata: parsed.Metadata,
				Origin: d.Origin, OriginRef: d.OriginRef, VerifyStatus: d.VerifyStatus,
				CreatedAt: store.Now(),
			}
			if err := q.InsertPackage(ctx, pkg); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if err := q.AddPackageBlobs(ctx, pkg.ID, d.Blobs); err != nil {
			return err
		}
		if err := q.Place(ctx, pkg.ID, dist, state, ownerConflict != ""); err != nil {
			return err
		}
		res.Package, res.State = pkg, state
		return in.tag(ctx, q, fm, parsed.Name, res, d.Tags, d.Owner)
	})
	if err != nil {
		return nil, err
	}
	if res.Moved && in.Publish != nil {
		in.Publish()
	}
	in.Log.Info("recorded", "package", ref, "tags", d.Tags, "state", res.State,
		"existing", res.Existing, "owner", d.Owner, "origin", d.Origin)
	return res, nil
}

// tag points tags at an active package. Res.Moved reports whether the
// next publish has something new to serve.
func (in *Ingester) tag(ctx context.Context, q *store.Queries, fm format.Format, name string,
	res *Result, tags []string, by string) error {
	if res.State != store.Active {
		return nil
	}
	if !res.Existing {
		res.Moved = true
	}
	for _, t := range tags {
		cur, err := q.GetTag(ctx, fm.Name(), name, t)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if cur != nil && cur.PackageID == res.Package.ID {
			continue
		}
		if err := q.SetTag(ctx, format.Tag{Format: fm.Name(), Name: name, Tag: t, PackageID: res.Package.ID}, by); err != nil {
			return err
		}
		res.Moved = true
	}
	return nil
}

// MayDeliver returns why owner may not deliver name in the format's
// namespace, or "" if it may. A push checks it before taking any data.
func (in *Ingester) MayDeliver(ctx context.Context, fm format.Format, name, owner string) (string, error) {
	var why string
	err := in.Store.Tx(ctx, func(q *store.Queries) error {
		var err error
		why, _, err = in.checkOwner(ctx, q, in.Config(), fm, name, owner)
		return err
	})
	return why, err
}
