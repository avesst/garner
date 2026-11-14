package db

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestOpenAppliesMigrations(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, filepath.Join(t.TempDir(), "garner.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	var name string
	if err := d.QueryRowContext(ctx, `SELECT name FROM repositories WHERE id = 'default'`).Scan(&name); err != nil {
		t.Fatalf("default repository: %v", err)
	}
	var fk int
	if err := d.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil || fk != 1 {
		t.Errorf("foreign_keys = %d, %v; want 1", fk, err)
	}
	var mode string
	if err := d.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Errorf("journal_mode = %q, %v; want wal", mode, err)
	}
}

func TestOpenTwiceIsLocked(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "garner.db")
	first, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	// flock locks belong to the open file, so a second Open in the same
	// process conflicts just like a second process would.
	if _, err := Open(ctx, path); !errors.Is(err, ErrLocked) {
		t.Fatalf("second Open: err = %v, want ErrLocked", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open after Close: %v", err)
	}
	again.Close()
}

func TestReopenKeepsData(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "garner.db")
	d, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(ctx, `INSERT INTO blobs (sha256, size, created_at) VALUES ('ab', 1, '2026-11-14T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	d.Close()

	d, err = Open(ctx, path) // migrations already applied: must be a no-op
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var n int
	if err := d.QueryRowContext(ctx, `SELECT count(*) FROM blobs`).Scan(&n); err != nil || n != 1 {
		t.Errorf("blobs after reopen = %d, %v; want 1", n, err)
	}
}

func TestForeignKeysEnforced(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, filepath.Join(t.TempDir(), "garner.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	_, err = d.ExecContext(ctx, `INSERT INTO placements (package_id, distribution, state, created_at)
		VALUES ('missing', 'stable', 'active', '2026-11-14T00:00:00Z')`)
	if err == nil {
		t.Error("placement for a missing package was accepted")
	}
}
