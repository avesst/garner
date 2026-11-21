package db

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
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

// Snapshots from before 00009 get the formats of the packages they hold.
func TestSnapshotFormatsBackfill(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, filepath.Join(t.TempDir(), "garner.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	sub, err := fs.Sub(migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	p, err := goose.NewProvider(goose.DialectSQLite3, d.DB, sub)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.DownTo(ctx, 8); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO blobs (sha256, size, created_at) VALUES ('ab', 1, '2026-11-14T00:00:00Z')`,
		`INSERT INTO packages (id, repository_id, format, name, version, blob_sha256, origin, origin_ref,
			verify_status, created_at) VALUES ('p1', 'default', 'deb', 'app', '1.0', 'ab', 'upload', 't',
			'n/a', '2026-11-14T00:00:00Z')`,
		`INSERT INTO snapshots (id, created_at, config_hash) VALUES ('s1', '2026-11-14T00:00:00Z', 'h')`,
		`INSERT INTO snapshots (id, created_at, config_hash) VALUES ('s2', '2026-11-14T00:00:01Z', 'h')`,
		`INSERT INTO snapshot_packages (snapshot_id, package_id, distribution) VALUES ('s1', 'p1', 'stable')`,
	} {
		if _, err := d.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.Up(ctx); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	rows, err := d.QueryContext(ctx, `SELECT snapshot_id, format FROM snapshot_formats`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, f string
		if err := rows.Scan(&id, &f); err != nil {
			t.Fatal(err)
		}
		got[id] += f
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["s1"] != "deb" {
		t.Errorf("snapshot_formats = %v, want s1 with deb only", got)
	}
}
