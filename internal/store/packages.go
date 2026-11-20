package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	"github.com/avesst/garner/internal/format"
)

// Placement states.
const (
	Active  = "active"
	Held    = "held"
	Removed = "removed"
)

// AddBlob records a blob and takes its row lock until the transaction
// ends. The caller must then make sure the file exists (blob.Store.Ensure):
// RemoveBlob, which takes the same lock, may have deleted it after Put
// found it present.
func (q *Queries) AddBlob(ctx context.Context, sha string, size int64) error {
	return q.lockBlob(ctx, sha, size)
}

// lockBlob creates the blob's row if it is missing, and locks it: an
// upsert that updates an existing row takes its row lock on PostgreSQL,
// and any write takes the database lock on SQLite.
func (q *Queries) lockBlob(ctx context.Context, sha string, size int64) error {
	_, err := q.q.ExecContext(ctx, `INSERT INTO blobs (sha256, size, created_at) VALUES (?, ?, ?)
		ON CONFLICT (sha256) DO UPDATE SET size = blobs.size`, sha, size, Now())
	return err
}

const packageColumns = `p.id, p.format, p.name, p.epoch, p.version, p.release, p.arch,
	p.blob_sha256, p.signed_blob_sha256, p.metadata, p.origin, p.origin_ref,
	p.verify_status, p.pinned, p.created_at`

type scanner interface{ Scan(dest ...any) error }

func scanPackage(s scanner, extra ...any) (*format.Package, error) {
	var p format.Package
	var signed sql.NullString
	var meta string
	dest := []any{&p.ID, &p.Format, &p.Name, &p.Epoch, &p.Version, &p.Release, &p.Arch,
		&p.BlobSHA256, &signed, &meta, &p.Origin, &p.OriginRef, &p.VerifyStatus, &p.Pinned, &p.CreatedAt}
	if err := s.Scan(append(dest, extra...)...); err != nil {
		return nil, err
	}
	p.SignedSHA256 = nullString(signed)
	p.Metadata = json.RawMessage(meta)
	return &p, nil
}

// Identity is a package's identity within its format.
type Identity struct {
	Format  string
	Name    string
	Epoch   int
	Version string
	Release string
	Arch    string
}

// FindPackage returns the package with this identity and original blob.
func (q *Queries) FindPackage(ctx context.Context, id Identity, blob string) (*format.Package, error) {
	row := q.q.QueryRowContext(ctx, `SELECT `+packageColumns+` FROM packages p
		WHERE p.repository_id = ? AND p.format = ? AND p.name = ? AND p.epoch = ?
		AND p.version = ? AND p.release = ? AND p.arch = ? AND p.blob_sha256 = ?`,
		RepositoryID, id.Format, id.Name, id.Epoch, id.Version, id.Release, id.Arch, blob)
	p, err := scanPackage(row)
	return p, wrapNotFound(err, "package")
}

// Variant is a placed package with the reason it may be held.
type Variant struct {
	*format.Package
	// OwnerConflict marks a placement held because its deliverer did not
	// own the name (section 8).
	OwnerConflict bool
}

// DecidesVersion reports whether the variant takes part in the content
// check of design section 7: a hold of someone who does not own the name
// does not, until it is approved (section 8).
func (v Variant) DecidesVersion() bool { return !v.OwnerConflict || v.State != Held }

// PlacedVariants returns the packages with this identity that are placed,
// and not removed, in a distribution. The content check of design section
// 7 compares their blobs with a new delivery.
func (q *Queries) PlacedVariants(ctx context.Context, id Identity, dist string) ([]Variant, error) {
	rows, err := q.q.QueryContext(ctx, `SELECT `+packageColumns+`, pl.distribution, pl.state, pl.owner_conflict
		FROM packages p JOIN placements pl ON pl.package_id = p.id
		WHERE p.repository_id = ? AND p.format = ? AND p.name = ? AND p.epoch = ?
		AND p.version = ? AND p.release = ? AND p.arch = ? AND pl.distribution = ?
		AND pl.state <> ?`,
		RepositoryID, id.Format, id.Name, id.Epoch, id.Version, id.Release, id.Arch, dist, Removed)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Variant
	for rows.Next() {
		var v Variant
		var dist, state string
		p, err := scanPackage(rows, &dist, &state, &v.OwnerConflict)
		if err != nil {
			return nil, err
		}
		p.Distribution, p.State = dist, state
		v.Package = p
		out = append(out, v)
	}
	return out, rows.Err()
}

// ServedPrefixCollision reports whether another package of the format with
// the same name, version and architecture is served from a blob whose
// sha256 shares the 16-digit prefix used in pool paths (section 4).
func (q *Queries) ServedPrefixCollision(ctx context.Context, id Identity, served string) (bool, error) {
	rows, err := q.q.QueryContext(ctx, `SELECT blob_sha256, signed_blob_sha256 FROM packages
		WHERE repository_id = ? AND format = ? AND name = ? AND epoch = ? AND version = ?
		AND release = ? AND arch = ?`,
		RepositoryID, id.Format, id.Name, id.Epoch, id.Version, id.Release, id.Arch)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var blob string
		var signed sql.NullString
		if err := rows.Scan(&blob, &signed); err != nil {
			return false, err
		}
		other := blob
		if signed.Valid {
			other = signed.String
		}
		if other != served && other[:16] == served[:16] {
			return true, nil
		}
	}
	return false, rows.Err()
}

// InsertPackage stores a new package row; p.ID and p.CreatedAt must be set.
func (q *Queries) InsertPackage(ctx context.Context, p *format.Package) error {
	var signed any
	if p.SignedSHA256 != "" {
		signed = p.SignedSHA256
	}
	meta := string(p.Metadata)
	if meta == "" {
		meta = "{}"
	}
	_, err := q.q.ExecContext(ctx, `INSERT INTO packages (id, repository_id, format, name, epoch,
		version, release, arch, blob_sha256, signed_blob_sha256, metadata, origin, origin_ref,
		verify_status, pinned, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, RepositoryID, p.Format, p.Name, p.Epoch, p.Version, p.Release, p.Arch,
		p.BlobSHA256, signed, meta, p.Origin, p.OriginRef, p.VerifyStatus, p.Pinned, p.CreatedAt)
	return err
}

// SetSignedBlob replaces a package's served variant, after re-signing.
func (q *Queries) SetSignedBlob(ctx context.Context, id, sha string) error {
	_, err := q.q.ExecContext(ctx, `UPDATE packages SET signed_blob_sha256 = ? WHERE id = ?`, sha, id)
	return err
}

// Place puts a package into a distribution with the given state, or
// changes the state of an existing placement. ownerConflict marks a hold
// whose deliverer did not own the name (Variant).
func (q *Queries) Place(ctx context.Context, packageID, dist, state string, ownerConflict bool) error {
	_, err := q.q.ExecContext(ctx, `INSERT INTO placements (package_id, distribution, state, owner_conflict, created_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (package_id, distribution) DO UPDATE
		SET state = excluded.state, owner_conflict = excluded.owner_conflict`,
		packageID, dist, state, ownerConflict, Now())
	return err
}

// SetState changes the state of existing placements of a package. With
// dist "", every placement of the package changes. It returns the number
// of placements changed.
func (q *Queries) SetState(ctx context.Context, packageID, dist, state string) (int64, error) {
	query := `UPDATE placements SET state = ? WHERE package_id = ?`
	args := []any{state, packageID}
	if dist != "" {
		query += ` AND distribution = ?`
		args = append(args, dist)
	}
	res, err := q.q.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PackageFilter selects placed packages. Empty fields match everything.
type PackageFilter struct {
	ID           string
	Format       string
	Name         string
	Version      string // matched against the version as stored, without epoch
	Arch         string
	Distribution string
	States       []string
}

// Placed returns one row per placement matching f, with the served blob's
// size.
func (q *Queries) Placed(ctx context.Context, f PackageFilter) ([]*format.Package, error) {
	query := `SELECT ` + packageColumns + `, pl.distribution, pl.state, b.size
		FROM packages p JOIN placements pl ON pl.package_id = p.id
		JOIN blobs b ON b.sha256 = COALESCE(p.signed_blob_sha256, p.blob_sha256)
		WHERE p.repository_id = ?`
	args := []any{RepositoryID}
	add := func(col, v string) {
		if v != "" {
			query += " AND " + col + " = ?"
			args = append(args, v)
		}
	}
	add("p.id", f.ID)
	add("p.format", f.Format)
	add("p.name", f.Name)
	add("p.version", f.Version)
	add("p.arch", f.Arch)
	add("pl.distribution", f.Distribution)
	if len(f.States) > 0 {
		query += " AND pl.state IN (?" + strings.Repeat(", ?", len(f.States)-1) + ")"
		for _, s := range f.States {
			args = append(args, s)
		}
	}
	query += " ORDER BY p.format, p.name, pl.distribution, p.arch, p.created_at"
	rows, err := q.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*format.Package
	for rows.Next() {
		var dist, state string
		var size int64
		p, err := scanPackage(rows, &dist, &state, &size)
		if err != nil {
			return nil, err
		}
		p.Distribution, p.State, p.ServedSize = dist, state, size
		out = append(out, p)
	}
	return out, rows.Err()
}

func collectPlaced(rows *sql.Rows) ([]*format.Package, error) {
	defer rows.Close()
	var out []*format.Package
	for rows.Next() {
		var dist, state string
		p, err := scanPackage(rows, &dist, &state)
		if err != nil {
			return nil, err
		}
		p.Distribution, p.State = dist, state
		out = append(out, p)
	}
	return out, rows.Err()
}

// SignedPackages returns the packages of a format that have a re-signed
// variant, so they can be signed again after a key change.
func (q *Queries) SignedPackages(ctx context.Context, formatName string) ([]*format.Package, error) {
	rows, err := q.q.QueryContext(ctx, `SELECT `+packageColumns+` FROM packages p
		WHERE p.repository_id = ? AND p.format = ? AND p.signed_blob_sha256 IS NOT NULL`,
		RepositoryID, formatName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*format.Package
	for rows.Next() {
		p, err := scanPackage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// FormatCount is how many packages of a format are stored, and how many
// of those have a re-signed variant.
type FormatCount struct {
	Total, Signed int
}

// FormatCounts returns the stored packages per format.
func (q *Queries) FormatCounts(ctx context.Context) (map[string]FormatCount, error) {
	rows, err := q.q.QueryContext(ctx, `SELECT format, COUNT(*), COUNT(signed_blob_sha256)
		FROM packages WHERE repository_id = ? GROUP BY format`, RepositoryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]FormatCount{}
	for rows.Next() {
		var name string
		var c FormatCount
		if err := rows.Scan(&name, &c.Total, &c.Signed); err != nil {
			return nil, err
		}
		out[name] = c
	}
	return out, rows.Err()
}

// SignedWith returns the fingerprint of the key packages were last
// re-signed with, or "" if that is unknown.
func (q *Queries) SignedWith(ctx context.Context) (string, error) {
	var fp string
	err := q.q.QueryRowContext(ctx, `SELECT signed_with FROM repositories WHERE id = ?`, RepositoryID).Scan(&fp)
	return fp, err
}

// SetSignedWith records that every package is signed with fingerprint.
func (q *Queries) SetSignedWith(ctx context.Context, fingerprint string) error {
	_, err := q.q.ExecContext(ctx, `UPDATE repositories SET signed_with = ? WHERE id = ?`, fingerprint, RepositoryID)
	return err
}
