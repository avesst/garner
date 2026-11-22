package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// TagKey names a registry source's tag as garner serves it: the source,
// the image upstream, the local name and the tag (section 19).
type TagKey struct {
	Source, Repo, Name, Tag string
}

// SourceTag returns the upstream digest a tag was last applied from, or
// "" if none.
func (q *Queries) SourceTag(ctx context.Context, k TagKey) (string, error) {
	var d string
	err := q.q.QueryRowContext(ctx, `SELECT digest FROM source_tags
		WHERE source = ? AND repo = ? AND name = ? AND tag = ?`, k.Source, k.Repo, k.Name, k.Tag).Scan(&d)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return d, err
}

// SetSourceTag records that a tag was applied from an upstream digest.
func (q *Queries) SetSourceTag(ctx context.Context, k TagKey, digest string) error {
	_, err := q.q.ExecContext(ctx, `INSERT INTO source_tags (source, repo, name, tag, digest, applied_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (source, repo, name, tag) DO UPDATE SET digest = excluded.digest, applied_at = excluded.applied_at`,
		k.Source, k.Repo, k.Name, k.Tag, digest, Now())
	return err
}

// TagCandidate is a digest seen under a registry source's tag.
type TagCandidate struct {
	Digest    string
	FirstSeen time.Time
}

// AddTagCandidate records a digest seen under a tag, keeping the time it
// was first seen.
func (q *Queries) AddTagCandidate(ctx context.Context, k TagKey, digest string, at time.Time) error {
	_, err := q.q.ExecContext(ctx, `INSERT INTO source_tag_candidates (source, repo, name, tag, digest, first_seen)
		VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (source, repo, name, tag, digest) DO NOTHING`,
		k.Source, k.Repo, k.Name, k.Tag, digest, FormatTime(at))
	return err
}

// TagCandidates returns a tag's candidates, newest first.
func (q *Queries) TagCandidates(ctx context.Context, k TagKey) ([]TagCandidate, error) {
	rows, err := q.q.QueryContext(ctx, `SELECT digest, first_seen FROM source_tag_candidates
		WHERE source = ? AND repo = ? AND name = ? AND tag = ? ORDER BY first_seen DESC, digest`,
		k.Source, k.Repo, k.Name, k.Tag)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TagCandidate
	for rows.Next() {
		var c TagCandidate
		var at string
		if err := rows.Scan(&c.Digest, &at); err != nil {
			return nil, err
		}
		if c.FirstSeen, err = ParseTime(at); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DropTagCandidates forgets a tag's candidates first seen at or before
// upTo, and those first seen after from: older ones are never applied once
// a newer one was, and newer ones upstream has moved the tag away from
// were withdrawn. A zero bound is not applied.
func (q *Queries) DropTagCandidates(ctx context.Context, k TagKey, upTo, from time.Time) error {
	if !upTo.IsZero() {
		if _, err := q.q.ExecContext(ctx, `DELETE FROM source_tag_candidates
			WHERE source = ? AND repo = ? AND name = ? AND tag = ? AND first_seen <= ?`,
			k.Source, k.Repo, k.Name, k.Tag, FormatTime(upTo)); err != nil {
			return err
		}
	}
	if !from.IsZero() {
		if _, err := q.q.ExecContext(ctx, `DELETE FROM source_tag_candidates
			WHERE source = ? AND repo = ? AND name = ? AND tag = ? AND first_seen > ?`,
			k.Source, k.Repo, k.Name, k.Tag, FormatTime(from)); err != nil {
			return err
		}
	}
	return nil
}

// DropTagCandidate forgets one candidate, such as one upstream deleted.
func (q *Queries) DropTagCandidate(ctx context.Context, k TagKey, digest string) error {
	_, err := q.q.ExecContext(ctx, `DELETE FROM source_tag_candidates
		WHERE source = ? AND repo = ? AND name = ? AND tag = ? AND digest = ?`,
		k.Source, k.Repo, k.Name, k.Tag, digest)
	return err
}

// PackageBlobsOf returns the further blobs one package owns.
func (q *Queries) PackageBlobsOf(ctx context.Context, packageID string) ([]string, error) {
	rows, err := q.q.QueryContext(ctx, `SELECT sha256 FROM package_blobs WHERE package_id = ? ORDER BY sha256`, packageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			return nil, err
		}
		out = append(out, sha)
	}
	return out, rows.Err()
}

// BlobReferenced reports whether a package or a snapshot references a
// blob, which keeps it from GC without staging.
func (q *Queries) BlobReferenced(ctx context.Context, sha string) (bool, error) {
	var ref bool
	err := q.q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM packages
		WHERE blob_sha256 = ? OR signed_blob_sha256 = ?)
		OR EXISTS (SELECT 1 FROM snapshot_packages WHERE served_sha256 = ?)
		OR EXISTS (SELECT 1 FROM package_blobs WHERE sha256 = ?)`, sha, sha, sha, sha).Scan(&ref)
	return ref, err
}

// UnstageBlobs ends the stage of blobs a package now owns, so a held
// package's blobs are not usable by pushes.
func (q *Queries) UnstageBlobs(ctx context.Context, shas []string) error {
	for _, sha := range shas {
		if _, err := q.q.ExecContext(ctx, `DELETE FROM staged_blobs WHERE sha256 = ?`, sha); err != nil {
			return err
		}
	}
	return nil
}
