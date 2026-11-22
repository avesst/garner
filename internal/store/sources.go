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

// DropPending forgets a pending release, for one that is no longer listed.
func (q *Queries) DropPending(ctx context.Context, source, repo, releaseID string) error {
	_, err := q.q.ExecContext(ctx, `DELETE FROM pending_releases WHERE source = ? AND repo = ? AND release_id = ?`,
		source, repo, releaseID)
	return err
}

// SeenTag reports whether a release with this tag was processed.
func (q *Queries) SeenTag(ctx context.Context, source, repo, tag string) (bool, error) {
	var n int
	err := q.q.QueryRowContext(ctx, `SELECT count(*) FROM seen_releases
		WHERE source = ? AND repo = ? AND tag = ?`, source, repo, tag).Scan(&n)
	return n > 0, err
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

// AddAgeOverride records that a release may skip min_release_age. Repeating
// it for the same tag moves its time forward and records the new caller.
func (q *Queries) AddAgeOverride(ctx context.Context, source, repo, tag, by string, at time.Time) error {
	_, err := q.q.ExecContext(ctx, `INSERT INTO release_age_overrides (source, repo, tag, created_at, created_by)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (source, repo, tag) DO UPDATE SET created_at = excluded.created_at, created_by = excluded.created_by`,
		source, repo, tag, FormatTime(at), by)
	return err
}

// AgeOverride returns when a release was allowed to skip min_release_age;
// ok is false when it was not.
func (q *Queries) AgeOverride(ctx context.Context, source, repo, tag string) (t time.Time, ok bool, err error) {
	var at string
	err = q.q.QueryRowContext(ctx, `SELECT created_at FROM release_age_overrides
		WHERE source = ? AND repo = ? AND tag = ?`, source, repo, tag).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	t, err = ParseTime(at)
	return t, err == nil, err
}

// SourceRepos returns the cached repository listing of a source, sorted.
func (q *Queries) SourceRepos(ctx context.Context, source string) ([]string, error) {
	rows, err := q.q.QueryContext(ctx, `SELECT repo FROM source_repos WHERE source = ? ORDER BY repo`, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
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
