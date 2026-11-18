// Package ingest takes a package file from an upload or a sync and records
// it (design sections 7 and 8).
package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/avesst/garner/internal/blob"
	"github.com/avesst/garner/internal/config"
	"github.com/avesst/garner/internal/format"
	"github.com/avesst/garner/internal/id"
	"github.com/avesst/garner/internal/store"
)

// Kind classifies an ingest error for the API.
type Kind int

const (
	// Invalid: the file or request is wrong (HTTP 400).
	Invalid Kind = iota + 1
	// Conflict: a content or ownership conflict (HTTP 409).
	Conflict
	// NotConfigured: the distribution or architecture is not configured
	// (HTTP 400 for uploads; sync lists the asset as skipped).
	NotConfigured
)

// Error is an ingest failure the caller can report to the uploader.
type Error struct {
	Kind Kind
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

func invalid(format string, args ...any) error {
	return &Error{Kind: Invalid, Msg: fmt.Sprintf(format, args...)}
}

func notConfigured(format string, args ...any) error {
	return &Error{Kind: NotConfigured, Msg: fmt.Sprintf(format, args...)}
}

func conflict(format string, args ...any) error {
	return &Error{Kind: Conflict, Msg: fmt.Sprintf(format, args...)}
}

// Ingester runs the common pipeline.
type Ingester struct {
	Store   *store.Store
	Blobs   *blob.Store
	Formats *format.Registry
	Config  func() *config.Config
	// TmpDir holds scratch files for Prepare; it must be on any
	// filesystem (results are copied into the blob store).
	TmpDir string
	// Publish requests a publish; it must not block.
	Publish func()
	Log     *slog.Logger
}

// Request describes one delivery.
type Request struct {
	// Targets maps a format's DistributionKey to the distribution to
	// place the package in ("suite": "noble"). A missing or empty entry
	// means the default distribution (format.DefaultDistribution).
	Targets map[string]string
	// Owner is the delivering identity: a token's owner label or
	// "source:<name>".
	Owner        string
	Origin       string // "upload" or "source"
	OriginRef    string
	VerifyStatus string
	// Held stores the package held for approval (approval: manual).
	Held bool
	// HoldOnOwnerConflict stores a package whose name belongs to someone
	// else as held and records a conflict, as sync does (section 8).
	// Without it the delivery is rejected and nothing is recorded.
	HoldOnOwnerConflict bool
}

// Result describes what happened.
type Result struct {
	Package      *format.Package
	Distribution string
	State        string
	// Existing is true when the same file was already in the
	// distribution; nothing changed.
	Existing bool
	// Conflict explains why a package was stored held.
	Conflict string
}

// SourceOwner is the owner label of a sync source.
func SourceOwner(source string) string { return "source:" + source }

// Ingest stores the file read from r and records it.
func (in *Ingester) Ingest(ctx context.Context, r io.Reader, req Request) (*Result, error) {
	cfg := in.Config()
	b, f, err := in.Blobs.PutOpen(r)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	fm, err := in.Formats.Detect(f)
	if err != nil {
		return nil, invalid("%v", err)
	}
	parsed, err := fm.Parse(f)
	if err != nil {
		return nil, invalid("%s: %v", fm.Name(), err)
	}

	dist := req.Targets[fm.DistributionKey()]
	if dist == "" {
		dist = format.DefaultDistribution(fm.Distributions(cfg))
	}
	if dists := fm.Distributions(cfg); !slices.Contains(dists, dist) {
		return nil, notConfigured("%s distribution %q is not configured; configured: %s",
			fm.Name(), dist, strings.Join(dists, ", "))
	}
	if archs := fm.Architectures(cfg); parsed.Arch != fm.ArchIndependent() && !slices.Contains(archs, parsed.Arch) {
		return nil, notConfigured("%s architecture %q is not configured; configured: %s",
			fm.Name(), parsed.Arch, strings.Join(archs, ", "))
	}

	ident := store.Identity{Format: fm.Name(), Name: parsed.Name, Epoch: parsed.Epoch,
		Version: parsed.Version, Release: parsed.Release, Arch: parsed.Arch}
	ref := ref(fm.Name(), parsed)

	// Re-signing runs an external tool, so it happens before the
	// transaction and only for content garner has not seen.
	existing, err := in.Store.FindPackage(ctx, ident, b.SHA256)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	var signed *prepared
	defer func() { signed.close() }()
	prepare := func() (err error) {
		// Prepare reads the blob by name; GC may have removed it.
		if err := in.Blobs.Ensure(b.SHA256, f, b.Size); err != nil {
			return err
		}
		signed, err = in.prepare(ctx, fm, b.SHA256)
		return err
	}
	if existing == nil {
		if err := prepare(); err != nil {
			return nil, err
		}
	}

	res := &Result{Distribution: dist}
	tx := func(q *store.Queries) error {
		res.Existing, res.Conflict, res.State = false, "", ""
		variants, err := q.PlacedVariants(ctx, ident, dist)
		if err != nil {
			return err
		}
		for _, v := range variants {
			if v.BlobSHA256 == b.SHA256 {
				res.Package, res.State, res.Existing = v, v.State, true
				return nil
			}
		}
		if len(variants) > 0 {
			return conflict("%s is already in %s with different content (sha256 %s)",
				ref, dist, variants[0].BlobSHA256)
		}

		state := store.Active
		if req.Held {
			state = store.Held
		}
		ownerConflict, err := in.checkOwner(ctx, q, cfg, fm.Namespace(), parsed.Name, req.Owner)
		if err != nil {
			return err
		}
		// Replacing a package on clients needs its name as much as
		// shipping it does; a free name is claimed the same way.
		for _, name := range parsed.Replaces {
			if ownerConflict != "" {
				break
			}
			if name == parsed.Name {
				continue
			}
			c, err := in.checkOwner(ctx, q, cfg, fm.Namespace(), name, req.Owner)
			if err != nil {
				return err
			}
			if c != "" {
				ownerConflict = "replaces another owner's package: " + c
			}
		}
		if ownerConflict != "" {
			if !req.HoldOnOwnerConflict {
				return conflict("%s", ownerConflict)
			}
			state = store.Held
			res.Conflict = ownerConflict
			if err := q.AddConflict(ctx, id.New(), "ownership", ref, ownerConflict); err != nil {
				return err
			}
		}

		pkg, err := q.FindPackage(ctx, ident, b.SHA256)
		if errors.Is(err, store.ErrNotFound) {
			if existing != nil {
				return errPackageGone
			}
			if pkg, err = in.insert(ctx, q, fm, parsed, b, f, signed, req); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if err := q.Place(ctx, pkg.ID, dist, state); err != nil {
			return err
		}
		res.Package, res.State = pkg, state
		return nil
	}
	err = in.Store.Tx(ctx, tx)
	if errors.Is(err, errPackageGone) {
		// GC deleted the package found above, which skipped re-signing:
		// prepare it as new content and try once more.
		existing = nil
		if err := prepare(); err != nil {
			return nil, err
		}
		err = in.Store.Tx(ctx, tx)
	}
	if err != nil {
		return nil, err
	}
	if !res.Existing && res.State == store.Active && in.Publish != nil {
		in.Publish()
	}
	in.Log.Info("ingested", "package", ref, "distribution", dist, "state", res.State,
		"existing", res.Existing, "owner", req.Owner, "origin", req.Origin)
	return res, nil
}

// errPackageGone means a package found before the transaction was deleted
// by GC before it started.
var errPackageGone = errors.New("package deleted during ingest")

// insert records a new package. Each blob is recorded under its row lock
// and then restored if GC removed its file since Put (store.AddBlob).
func (in *Ingester) insert(ctx context.Context, q *store.Queries, fm format.Format, parsed *format.Parsed,
	b blob.Blob, f *os.File, signed *prepared, req Request) (*format.Package, error) {
	if err := q.AddBlob(ctx, b.SHA256, b.Size); err != nil {
		return nil, err
	}
	if err := in.Blobs.Ensure(b.SHA256, f, b.Size); err != nil {
		return nil, err
	}
	pkg := &format.Package{
		ID: id.New(), Format: fm.Name(), Name: parsed.Name, Epoch: parsed.Epoch,
		Version: parsed.Version, Release: parsed.Release, Arch: parsed.Arch,
		BlobSHA256: b.SHA256, Metadata: parsed.Metadata,
		Origin: req.Origin, OriginRef: req.OriginRef, VerifyStatus: req.VerifyStatus,
		CreatedAt: store.Now(),
	}
	if signed != nil {
		if err := signed.record(ctx, q, in.Blobs); err != nil {
			return nil, err
		}
		pkg.SignedSHA256 = signed.SHA256
	}
	ident := store.Identity{Format: pkg.Format, Name: pkg.Name, Epoch: pkg.Epoch,
		Version: pkg.Version, Release: pkg.Release, Arch: pkg.Arch}
	if clash, err := q.ServedPrefixCollision(ctx, ident, pkg.ServedSHA256()); err != nil {
		return nil, err
	} else if clash {
		return nil, conflict("pool path collision: another file of %s shares the hash prefix %s",
			ref(fm.Name(), parsed), pkg.ServedSHA256()[:16])
	}
	if err := q.InsertPackage(ctx, pkg); err != nil {
		return nil, err
	}
	return pkg, nil
}

// prepared is a blob written by a format's Prepare. Its file stays open,
// so the blob can be restored if GC removes it before it is recorded.
type prepared struct {
	blob.Blob
	f *os.File
}

func (p *prepared) close() {
	if p != nil {
		p.f.Close()
	}
}

// record records the blob under its row lock and restores its file if GC
// removed it (store.AddBlob).
func (p *prepared) record(ctx context.Context, q *store.Queries, blobs *blob.Store) error {
	if err := q.AddBlob(ctx, p.SHA256, p.Size); err != nil {
		return err
	}
	return blobs.Ensure(p.SHA256, p.f, p.Size)
}

// prepare runs the format's Prepare and stores its output as a blob. It
// returns nil when the format leaves the package as it is.
func (in *Ingester) prepare(ctx context.Context, fm format.Format, sha string) (*prepared, error) {
	orig, err := in.Blobs.Path(sha)
	if err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(in.TmpDir, "prepare-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	out, err := fm.Prepare(ctx, orig, tmp)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", fm.Name(), err)
	}
	if out == "" {
		return nil, nil
	}
	f, err := os.Open(out)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, bf, err := in.Blobs.PutOpen(f)
	if err != nil {
		return nil, err
	}
	return &prepared{Blob: b, f: bf}, nil
}

// checkOwner applies section 8. It returns a non-empty explanation when
// the name belongs to someone else, and assigns the name when it is free.
func (in *Ingester) checkOwner(ctx context.Context, q *store.Queries, cfg *config.Config,
	namespace, name, owner string) (string, error) {
	if reserved := ReservedOwner(cfg, name); reserved != "" {
		if reserved != owner {
			return fmt.Sprintf("name %q is reserved for %s", name, reserved), nil
		}
		cur, err := q.Owner(ctx, namespace, name)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return "", err
		}
		if cur == nil || cur.Owner != owner {
			return "", q.SetOwner(ctx, namespace, name, owner, store.OwnerConfig)
		}
		return "", nil
	}
	cur, err := q.Owner(ctx, namespace, name)
	if errors.Is(err, store.ErrNotFound) {
		return "", q.SetOwner(ctx, namespace, name, owner, store.OwnerFirst)
	}
	if err != nil {
		return "", err
	}
	if cur.Owner != owner {
		return fmt.Sprintf("name %q is owned by %s", name, cur.Owner), nil
	}
	return "", nil
}

// ReservedOwner returns the source owner whose owns: globs match name, or
// "". Validation guarantees at most one source matches.
func ReservedOwner(cfg *config.Config, name string) string {
	for _, s := range cfg.Sources {
		for _, g := range s.Owns {
			if ok, _ := path.Match(g, name); ok {
				return SourceOwner(s.Name)
			}
		}
	}
	return ""
}

// ref is a human-readable package reference for messages and conflicts.
func ref(formatName string, p *format.Parsed) string {
	v := p.Version
	if p.Release != "" {
		v += "-" + p.Release
	}
	if p.Epoch != 0 {
		v = fmt.Sprintf("%d:%s", p.Epoch, v)
	}
	return fmt.Sprintf("%s %s %s %s", formatName, p.Name, v, p.Arch)
}

// Resign prepares every package that has a served variant again from its
// original, for example after the signing key changed (section 10). It
// returns how many packages changed.
func (in *Ingester) Resign(ctx context.Context) (int, error) {
	n := 0
	for _, fm := range in.Formats.All() {
		pkgs, err := in.Store.SignedPackages(ctx, fm.Name())
		if err != nil {
			return n, err
		}
		for _, p := range pkgs {
			signed, err := in.prepare(ctx, fm, p.BlobSHA256)
			if err != nil {
				return n, fmt.Errorf("%s %s: %w", fm.Name(), p.Name, err)
			}
			if signed == nil || signed.SHA256 == p.SignedSHA256 {
				signed.close()
				continue
			}
			err = in.Store.Tx(ctx, func(q *store.Queries) error {
				if err := signed.record(ctx, q, in.Blobs); err != nil {
					return err
				}
				return q.SetSignedBlob(ctx, p.ID, signed.SHA256)
			})
			signed.close()
			if err != nil {
				return n, err
			}
			n++
		}
	}
	return n, nil
}
