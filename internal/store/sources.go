package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// SourceState is one row of source_state.
type SourceState struct {
	Source      string
	LastRun     string
	LastSuccess string
	LastError   string
	ETags       map[string]string
}

// SourceState returns a source's state; a source that never ran has an
// empty state, not an error.
func (q *Queries) SourceState(ctx context.Context, source string) (*SourceState, error) {
	st := &SourceState{Source: source, ETags: map[string]string{}}
	var run, ok, errText sql.NullString
	var etags string
	err := q.q.QueryRowContext(ctx, `SELECT last_run, last_success, last_error, etags FROM source_state
		WHERE source = ?`, source).Scan(&run, &ok, &errText, &etags)
	if errors.Is(err, sql.ErrNoRows) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	st.LastRun, st.LastSuccess, st.LastError = nullString(run), nullString(ok), nullString(errText)
	if err := json.Unmarshal([]byte(etags), &st.ETags); err != nil || st.ETags == nil {
		st.ETags = map[string]string{}
	}
	return st, nil
}

// SaveSourceState stores a source's state.
func (q *Queries) SaveSourceState(ctx context.Context, st *SourceState) error {
	etags, err := json.Marshal(st.ETags)
	if err != nil {
		return err
	}
	null := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	_, err = q.q.ExecContext(ctx, `INSERT INTO source_state (source, last_run, last_success, last_error, etags)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (source) DO UPDATE SET last_run = excluded.last_run, last_success = excluded.last_success,
		last_error = excluded.last_error, etags = excluded.etags`,
		st.Source, null(st.LastRun), null(st.LastSuccess), null(st.LastError), string(etags))
	return err
}

// SeenRelease reports whether a release was processed.
func (q *Queries) SeenRelease(ctx context.Context, source, repo, releaseID string) (bool, error) {
	var n int
	err := q.q.QueryRowContext(ctx, `SELECT count(*) FROM seen_releases
		WHERE source = ? AND repo = ? AND release_id = ?`, source, repo, releaseID).Scan(&n)
	return n > 0, err
}

// LastProcessed returns the latest publish time of a processed release of
// a repository; ok is false when none was processed.
func (q *Queries) LastProcessed(ctx context.Context, source, repo string) (t time.Time, ok bool, err error) {
	var n int
	var latest sql.NullString
	err = q.q.QueryRowContext(ctx, `SELECT count(*), max(published_at) FROM seen_releases
		WHERE source = ? AND repo = ?`, source, repo).Scan(&n, &latest)
	if err != nil || n == 0 {
		return time.Time{}, false, err
	}
	if latest.Valid && latest.String != "" {
		t, err = ParseTime(latest.String)
	}
	return t, true, err
}

// MarkSeen records a processed release; it is no longer pending.
func (q *Queries) MarkSeen(ctx context.Context, source, repo, tag, releaseID string, publishedAt time.Time) error {
	pub := ""
	if !publishedAt.IsZero() {
		pub = FormatTime(publishedAt)
	}
	_, err := q.q.ExecContext(ctx, `INSERT INTO seen_releases (source, repo, tag, release_id, processed_at, published_at)
		VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (source, repo, release_id) DO NOTHING`,
		source, repo, tag, releaseID, Now(), pub)
	if err != nil {
		return err
	}
	_, err = q.q.ExecContext(ctx, `DELETE FROM pending_releases WHERE source = ? AND repo = ? AND release_id = ?`,
		source, repo, releaseID)
	return err
}

// MarkPending records a release whose attempt failed with an error worth
// retrying.
func (q *Queries) MarkPending(ctx context.Context, source, repo, releaseID string, publishedAt time.Time) error {
	pub := ""
	if !publishedAt.IsZero() {
		pub = FormatTime(publishedAt)
	}
	_, err := q.q.ExecContext(ctx, `INSERT INTO pending_releases (source, repo, release_id, published_at)
		VALUES (?, ?, ?, ?) ON CONFLICT (source, repo, release_id) DO NOTHING`,
		source, repo, releaseID, pub)
	return err
}

// PendingReleases returns the IDs of a repository's pending releases and
// the earliest publish time among them (zero when none has one).
func (q *Queries) PendingReleases(ctx context.Context, source, repo string) (map[string]bool, time.Time, error) {
	rows, err := q.q.QueryContext(ctx, `SELECT release_id, published_at FROM pending_releases
		WHERE source = ? AND repo = ?`, source, repo)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer rows.Close()
	ids := map[string]bool{}
	var oldest time.Time
	for rows.Next() {
		var id, pub string
		if err := rows.Scan(&id, &pub); err != nil {
			return nil, time.Time{}, err
		}
		ids[id] = true
		if pub == "" {
			continue
		}
		t, err := ParseTime(pub)
		if err != nil {
			return nil, time.Time{}, err
		}
		if oldest.IsZero() || t.Before(oldest) {
			oldest = t
		}
	}
	return ids, oldest, rows.Err()
}

// SaveRepos replaces the cached repository listing of a source.
func (q *Queries) SaveRepos(ctx context.Context, source string, repos []string) error {
	if _, err := q.q.ExecContext(ctx, `DELETE FROM source_repos WHERE source = ?`, source); err != nil {
		return err
	}
	now := Now()
	for _, r := range repos {
		if _, err := q.q.ExecContext(ctx, `INSERT INTO source_repos (source, repo, last_seen) VALUES (?, ?, ?)`,
			source, r, now); err != nil {
			return err
		}
	}
	return nil
}
