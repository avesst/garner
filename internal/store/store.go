// Package store holds garner's database queries (design section 6). Every
// query is portable SQL; versions are never sorted by the database.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// RepositoryID is the only repository in the MVP.
const RepositoryID = "default"

// ErrNotFound is returned when a looked-up row does not exist.
var ErrNotFound = errors.New("not found")

type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Queries runs queries on the database or inside a transaction.
type Queries struct {
	q querier
}

// Store is the database.
type Store struct {
	Queries
	db *sql.DB
}

// New wraps an open database.
func New(db *sql.DB) *Store {
	return &Store{Queries: Queries{q: db}, db: db}
}

// Tx runs fn in a transaction, committing if it returns nil.
func (s *Store) Tx(ctx context.Context, fn func(q *Queries) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(&Queries{q: tx}); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Ping checks that the database answers.
func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

// timeFormat sorts lexically in time order, unlike RFC3339Nano, which
// drops trailing zeros.
const timeFormat = "2006-01-02T15:04:05.000000Z"

// Now returns the current time as stored in the database.
func Now() string { return FormatTime(time.Now()) }

// FormatTime formats t as stored in the database.
func FormatTime(t time.Time) string { return t.UTC().Format(timeFormat) }

// ParseTime parses a stored time.
func ParseTime(s string) (time.Time, error) { return time.Parse(timeFormat, s) }

func nullString(s sql.NullString) string {
	if s.Valid {
		return s.String
	}
	return ""
}

func wrapNotFound(err error, what string) error {
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%s: %w", what, ErrNotFound)
	}
	return err
}
