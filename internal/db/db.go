// Package db opens garner's SQLite database, guards it against a second
// process, and applies schema migrations (design sections 3, 6 and 12).
package db

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"syscall"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

//go:embed migrations/*.sql
var migrations embed.FS

// ErrLocked means another garner process holds the database.
var ErrLocked = errors.New("database is in use by another garner process")

// DB is an open database plus the lock that makes this process its only
// user.
type DB struct {
	*sql.DB
	lock *os.File
}

// Open takes the lock on path+".lock", opens the SQLite database at path and
// applies pending migrations. The lock is an flock, so it is released when
// the process exits, even after a crash.
func Open(ctx context.Context, path string) (*DB, error) {
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%s: %w", path, ErrLocked)
		}
		return nil, fmt.Errorf("lock %s.lock: %w", path, err)
	}

	sqlDB, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		lock.Close()
		return nil, err
	}
	d := &DB{DB: sqlDB, lock: lock}
	if err := d.migrate(ctx); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

// dsn sets per-connection pragmas. synchronous=FULL makes a committed
// transaction survive power loss: an acknowledged upload must not vanish.
func dsn(path string) string {
	q := url.Values{}
	for _, p := range []string{
		"foreign_keys(1)",
		"journal_mode(WAL)",
		"synchronous(FULL)",
		"busy_timeout(5000)",
	} {
		q.Add("_pragma", p)
	}
	return "file:" + path + "?" + q.Encode()
}

func (d *DB) migrate(ctx context.Context) error {
	sub, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return err
	}
	p, err := goose.NewProvider(goose.DialectSQLite3, d.DB, sub)
	if err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

// Close closes the database and releases the lock.
func (d *DB) Close() error {
	err := d.DB.Close()
	if lerr := d.lock.Close(); err == nil {
		err = lerr
	}
	return err
}
