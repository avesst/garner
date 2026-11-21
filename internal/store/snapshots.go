package store

import (
	"context"
	"database/sql"
)

// Snapshot is one row of snapshots.
type Snapshot struct {
	ID         string `json:"id"`
	CreatedAt  string `json:"created_at"`
	ConfigHash string `json:"config_hash"`
	IsCurrent  bool   `json:"current"`
	// Inputs is a digest of what the snapshot was rendered from.
	Inputs string `json:"-"`
	// ReplacedAt is when the snapshot stopped being current; "" if it is
	// current or never was.
	ReplacedAt string `json:"-"`
	// Formats are the formats the snapshot rendered. Only InsertSnapshot
	// uses it; SnapshotFormats reads it back.
	Formats []string `json:"-"`
}

// SnapshotEntry places a package in a distribution of a snapshot.
type SnapshotEntry struct {
	PackageID    string
	Distribution string
	// ServedSHA256 is the blob the snapshot serves the package from. A
	// re-sign can change the package's blob later; GC keeps this one.
	ServedSHA256 string
}

// InsertSnapshot records a snapshot, its formats and its package set.
func (q *Queries) InsertSnapshot(ctx context.Context, s *Snapshot, entries []SnapshotEntry) error {
	if _, err := q.q.ExecContext(ctx, `INSERT INTO snapshots (id, created_at, config_hash, is_current, inputs)
		VALUES (?, ?, ?, ?, ?)`, s.ID, s.CreatedAt, s.ConfigHash, false, s.Inputs); err != nil {
		return err
	}
	for _, f := range s.Formats {
		if _, err := q.q.ExecContext(ctx, `INSERT INTO snapshot_formats (snapshot_id, format)
			VALUES (?, ?)`, s.ID, f); err != nil {
			return err
		}
	}
	for _, e := range entries {
		if _, err := q.q.ExecContext(ctx, `INSERT INTO snapshot_packages (snapshot_id, package_id, distribution, served_sha256)
			VALUES (?, ?, ?, ?)`, s.ID, e.PackageID, e.Distribution, e.ServedSHA256); err != nil {
			return err
		}
	}
	return nil
}

// SetCurrent marks one snapshot current, and the one it replaces as
// replaced now.
func (q *Queries) SetCurrent(ctx context.Context, id string) error {
	if _, err := q.q.ExecContext(ctx, `UPDATE snapshots SET is_current = FALSE, replaced_at = ?
		WHERE is_current`, Now()); err != nil {
		return err
	}
	_, err := q.q.ExecContext(ctx, `UPDATE snapshots SET is_current = TRUE, replaced_at = '' WHERE id = ?`, id)
	return err
}

// Snapshots lists snapshots, newest first.
func (q *Queries) Snapshots(ctx context.Context) ([]Snapshot, error) {
	rows, err := q.q.QueryContext(ctx, `SELECT id, created_at, config_hash, is_current, inputs, replaced_at FROM snapshots
		ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Snapshot
	for rows.Next() {
		var s Snapshot
		if err := rows.Scan(&s.ID, &s.CreatedAt, &s.ConfigHash, &s.IsCurrent, &s.Inputs, &s.ReplacedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// CurrentSnapshot returns the current snapshot, or ErrNotFound.
func (q *Queries) CurrentSnapshot(ctx context.Context) (*Snapshot, error) {
	var s Snapshot
	err := q.q.QueryRowContext(ctx, `SELECT id, created_at, config_hash, is_current, inputs, replaced_at FROM snapshots
		WHERE is_current`).Scan(&s.ID, &s.CreatedAt, &s.ConfigHash, &s.IsCurrent, &s.Inputs, &s.ReplacedAt)
	if err != nil {
		return nil, wrapNotFound(err, "current snapshot")
	}
	return &s, nil
}

// SnapshotEntries returns a snapshot's package set.
func (q *Queries) SnapshotEntries(ctx context.Context, id string) ([]SnapshotEntry, error) {
	rows, err := q.q.QueryContext(ctx, `SELECT package_id, distribution FROM snapshot_packages
		WHERE snapshot_id = ?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SnapshotEntry
	for rows.Next() {
		var e SnapshotEntry
		if err := rows.Scan(&e.PackageID, &e.Distribution); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// SnapshotFormats returns the formats a snapshot rendered.
func (q *Queries) SnapshotFormats(ctx context.Context, id string) (map[string]bool, error) {
	rows, err := q.q.QueryContext(ctx, `SELECT format FROM snapshot_formats WHERE snapshot_id = ?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			return nil, err
		}
		out[f] = true
	}
	return out, rows.Err()
}

// SnapshotExists reports whether a snapshot row exists.
func (q *Queries) SnapshotExists(ctx context.Context, id string) (bool, error) {
	var n int
	err := q.q.QueryRowContext(ctx, `SELECT count(*) FROM snapshots WHERE id = ?`, id).Scan(&n)
	return n > 0, err
}

// DeleteSnapshot removes a snapshot row and its package set.
func (q *Queries) DeleteSnapshot(ctx context.Context, id string) error {
	_, err := q.q.ExecContext(ctx, `DELETE FROM snapshots WHERE id = ?`, id)
	return err
}

// ReferencedBlobs returns every blob a snapshot serves, and every blob
// referenced by a package that is in a snapshot or not removed everywhere.
// GC keeps these (section 9).
func (q *Queries) ReferencedBlobs(ctx context.Context) (map[string]bool, error) {
	rows, err := q.q.QueryContext(ctx, `SELECT p.blob_sha256, p.signed_blob_sha256 FROM packages p
		WHERE p.id IN (SELECT package_id FROM snapshot_packages)
		   OR p.id IN (SELECT package_id FROM placements WHERE state <> ?)
		UNION SELECT served_sha256, NULL FROM snapshot_packages WHERE served_sha256 <> ''`, Removed)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var blob string
		var signed sql.NullString
		if err := rows.Scan(&blob, &signed); err != nil {
			return nil, err
		}
		out[blob] = true
		if signed.Valid {
			out[signed.String] = true
		}
	}
	return out, rows.Err()
}

// Blobs returns every recorded blob with its creation time.
func (q *Queries) Blobs(ctx context.Context) (map[string]string, error) {
	rows, err := q.q.QueryContext(ctx, `SELECT sha256, created_at FROM blobs`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var sha, at string
		if err := rows.Scan(&sha, &at); err != nil {
			return nil, err
		}
		out[sha] = at
	}
	return out, rows.Err()
}

// DeleteUnreferencedPackages removes packages that are in no snapshot and
// whose placements are all removed. It returns the sha256s of the blobs no
// package or snapshot references any more. Their rows stay: a blob's row
// and file go together in RemoveBlob's transaction, so an upload of the
// same content in the meantime keeps them.
func (q *Queries) DeleteUnreferencedPackages(ctx context.Context) ([]string, error) {
	if _, err := q.q.ExecContext(ctx, `DELETE FROM placements WHERE package_id IN (
		SELECT p.id FROM packages p
		WHERE p.id NOT IN (SELECT package_id FROM snapshot_packages)
		  AND p.id NOT IN (SELECT package_id FROM placements WHERE state <> ?))`, Removed); err != nil {
		return nil, err
	}
	if _, err := q.q.ExecContext(ctx, `DELETE FROM packages
		WHERE id NOT IN (SELECT package_id FROM snapshot_packages)
		  AND id NOT IN (SELECT package_id FROM placements)`); err != nil {
		return nil, err
	}
	rows, err := q.q.QueryContext(ctx, `SELECT sha256 FROM blobs
		WHERE sha256 NOT IN (SELECT blob_sha256 FROM packages)
		  AND sha256 NOT IN (SELECT signed_blob_sha256 FROM packages WHERE signed_blob_sha256 IS NOT NULL)
		  AND sha256 NOT IN (SELECT served_sha256 FROM snapshot_packages)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var gone []string
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			return nil, err
		}
		gone = append(gone, sha)
	}
	return gone, rows.Err()
}

// RemoveBlob deletes the row of a blob no package or snapshot references,
// and calls remove, which deletes its file, before the transaction
// commits. It reports false, and leaves row and file alone, when one
// references the blob. It first takes the blob's row lock, which AddBlob
// also takes, so it waits for an upload that is adding a package with this
// content. Call it inside a transaction.
func (q *Queries) RemoveBlob(ctx context.Context, sha string, remove func() error) (bool, error) {
	if err := q.lockBlob(ctx, sha, 0); err != nil {
		return false, err
	}
	var referenced bool
	err := q.q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM packages
		WHERE blob_sha256 = ? OR signed_blob_sha256 = ?)
		OR EXISTS (SELECT 1 FROM snapshot_packages WHERE served_sha256 = ?)`, sha, sha, sha).Scan(&referenced)
	if err != nil || referenced {
		return false, err
	}
	if _, err := q.q.ExecContext(ctx, `DELETE FROM blobs WHERE sha256 = ?`, sha); err != nil {
		return false, err
	}
	return true, remove()
}
