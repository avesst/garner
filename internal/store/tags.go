package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/avesst/garner/internal/format"
)

// AddPackageBlobs records blobs a package owns besides its own blob
// (section 19). Each blob must have a row (AddBlob).
func (q *Queries) AddPackageBlobs(ctx context.Context, packageID string, shas []string) error {
	for _, sha := range shas {
		if _, err := q.q.ExecContext(ctx, `INSERT INTO package_blobs (package_id, sha256) VALUES (?, ?)
			ON CONFLICT (package_id, sha256) DO NOTHING`, packageID, sha); err != nil {
			return err
		}
	}
	return nil
}

// PackageBlobs returns the blobs each package owns besides its own blob,
// by package ID.
func (q *Queries) PackageBlobs(ctx context.Context) (map[string][]string, error) {
	rows, err := q.q.QueryContext(ctx, `SELECT package_id, sha256 FROM package_blobs ORDER BY package_id, sha256`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var id, sha string
		if err := rows.Scan(&id, &sha); err != nil {
			return nil, err
		}
		out[id] = append(out[id], sha)
	}
	return out, rows.Err()
}

// BlobSize returns the recorded size of a blob, or ErrNotFound.
func (q *Queries) BlobSize(ctx context.Context, sha string) (int64, error) {
	var n int64
	err := q.q.QueryRowContext(ctx, `SELECT size FROM blobs WHERE sha256 = ?`, sha).Scan(&n)
	return n, wrapNotFound(err, "blob")
}

// StageBlob keeps a blob that no package references yet from GC until
// expires, as for the layers of a push in progress (section 19). Staging
// again only extends the time. The blob must have a row (AddBlob).
func (q *Queries) StageBlob(ctx context.Context, sha string, expires time.Time) error {
	at := FormatTime(expires)
	_, err := q.q.ExecContext(ctx, `INSERT INTO staged_blobs (sha256, expires_at) VALUES (?, ?)
		ON CONFLICT (sha256) DO UPDATE SET expires_at = CASE
			WHEN excluded.expires_at > staged_blobs.expires_at THEN excluded.expires_at
			ELSE staged_blobs.expires_at END`, sha, at)
	return err
}

// Staged reports whether a blob is staged and not yet expired.
func (q *Queries) Staged(ctx context.Context, sha string, now time.Time) (bool, error) {
	var n int
	err := q.q.QueryRowContext(ctx, `SELECT count(*) FROM staged_blobs WHERE sha256 = ? AND expires_at > ?`,
		sha, FormatTime(now)).Scan(&n)
	return n > 0, err
}

// DeleteExpiredStaged ends the stage of blobs whose time has passed, so GC
// can collect those no package references.
func (q *Queries) DeleteExpiredStaged(ctx context.Context, now time.Time) (int64, error) {
	res, err := q.q.ExecContext(ctx, `DELETE FROM staged_blobs WHERE expires_at <= ?`, FormatTime(now))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SetTag points a tag at a package, creating or moving it.
func (q *Queries) SetTag(ctx context.Context, t format.Tag, by string) error {
	_, err := q.q.ExecContext(ctx, `INSERT INTO tags (repository_id, format, name, tag, package_id, updated_at, updated_by)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (repository_id, format, name, tag) DO UPDATE
		SET package_id = excluded.package_id, updated_at = excluded.updated_at, updated_by = excluded.updated_by`,
		RepositoryID, t.Format, t.Name, t.Tag, t.PackageID, Now(), by)
	return err
}

// GetTag returns a tag, or ErrNotFound.
func (q *Queries) GetTag(ctx context.Context, formatName, name, tag string) (*format.Tag, error) {
	t := format.Tag{Format: formatName, Name: name, Tag: tag}
	err := q.q.QueryRowContext(ctx, `SELECT package_id FROM tags
		WHERE repository_id = ? AND format = ? AND name = ? AND tag = ?`,
		RepositoryID, formatName, name, tag).Scan(&t.PackageID)
	if err != nil {
		return nil, wrapNotFound(err, "tag")
	}
	return &t, nil
}

// DeleteTag removes a tag. It reports whether the tag existed.
func (q *Queries) DeleteTag(ctx context.Context, formatName, name, tag string) (bool, error) {
	res, err := q.q.ExecContext(ctx, `DELETE FROM tags WHERE repository_id = ? AND format = ? AND name = ? AND tag = ?`,
		RepositoryID, formatName, name, tag)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// DeleteTagsOf removes every tag on a package.
func (q *Queries) DeleteTagsOf(ctx context.Context, packageID string) error {
	_, err := q.q.ExecContext(ctx, `DELETE FROM tags WHERE package_id = ?`, packageID)
	return err
}

// Tags returns every tag, ordered by format, name and tag.
func (q *Queries) Tags(ctx context.Context) ([]format.Tag, error) {
	return collectTags(q.q.QueryContext(ctx, `SELECT format, name, tag, package_id FROM tags
		WHERE repository_id = ? ORDER BY format, name, tag`, RepositoryID))
}

// SnapshotTags returns the tags a snapshot served.
func (q *Queries) SnapshotTags(ctx context.Context, id string) ([]format.Tag, error) {
	return collectTags(q.q.QueryContext(ctx, `SELECT format, name, tag, package_id FROM snapshot_tags
		WHERE snapshot_id = ? ORDER BY format, name, tag`, id))
}

// ReplaceTags sets the tags of the given formats to tags, for a rollback.
func (q *Queries) ReplaceTags(ctx context.Context, formats map[string]bool, tags []format.Tag, by string) error {
	for f := range formats {
		if _, err := q.q.ExecContext(ctx, `DELETE FROM tags WHERE repository_id = ? AND format = ?`,
			RepositoryID, f); err != nil {
			return err
		}
	}
	for _, t := range tags {
		if !formats[t.Format] {
			continue
		}
		if err := q.SetTag(ctx, t, by); err != nil {
			return err
		}
	}
	return nil
}

func collectTags(rows *sql.Rows, err error) ([]format.Tag, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []format.Tag
	for rows.Next() {
		var t format.Tag
		if err := rows.Scan(&t.Format, &t.Name, &t.Tag, &t.PackageID); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// PackageByID returns a package without placement.
func (q *Queries) PackageByID(ctx context.Context, id string) (*format.Package, error) {
	row := q.q.QueryRowContext(ctx, `SELECT `+packageColumns+` FROM packages p WHERE p.id = ?`, id)
	p, err := scanPackage(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, wrapNotFound(err, "package")
	}
	return p, err
}
