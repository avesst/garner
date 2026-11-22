package store

import (
	"context"
	"time"
)

// Upload is a blob upload session of a push (section 19).
type Upload struct {
	ID        string
	Format    string
	Name      string
	Size      int64
	CreatedAt string
	UpdatedAt string
}

// InsertUpload records a new, empty upload session.
func (q *Queries) InsertUpload(ctx context.Context, u *Upload) error {
	now := Now()
	u.CreatedAt, u.UpdatedAt = now, now
	_, err := q.q.ExecContext(ctx, `INSERT INTO uploads (id, repository_id, format, name, size, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, u.ID, RepositoryID, u.Format, u.Name, u.Size, now, now)
	return err
}

// GetUpload returns an upload session, or ErrNotFound.
func (q *Queries) GetUpload(ctx context.Context, id string) (*Upload, error) {
	var u Upload
	err := q.q.QueryRowContext(ctx, `SELECT id, format, name, size, created_at, updated_at FROM uploads
		WHERE id = ? AND repository_id = ?`, id, RepositoryID).
		Scan(&u.ID, &u.Format, &u.Name, &u.Size, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, wrapNotFound(err, "upload")
	}
	return &u, nil
}

// GrowUpload records that an upload grew from size from to size to. It
// reports false when the session is gone or another request changed its
// size first.
func (q *Queries) GrowUpload(ctx context.Context, id string, from, to int64) (bool, error) {
	res, err := q.q.ExecContext(ctx, `UPDATE uploads SET size = ?, updated_at = ? WHERE id = ? AND size = ?`,
		to, Now(), id, from)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// DeleteUpload removes an upload session's row.
func (q *Queries) DeleteUpload(ctx context.Context, id string) error {
	_, err := q.q.ExecContext(ctx, `DELETE FROM uploads WHERE id = ?`, id)
	return err
}

// Uploads returns every upload session's ID with when it last changed.
func (q *Queries) Uploads(ctx context.Context) (map[string]time.Time, error) {
	rows, err := q.q.QueryContext(ctx, `SELECT id, updated_at FROM uploads`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var id, at string
		if err := rows.Scan(&id, &at); err != nil {
			return nil, err
		}
		t, err := ParseTime(at)
		if err != nil {
			return nil, err
		}
		out[id] = t
	}
	return out, rows.Err()
}
