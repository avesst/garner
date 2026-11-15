-- A digest of what a snapshot was rendered from, so that a start can skip
-- publishing when nothing changed (design section 9). Existing snapshots
-- get '' and are republished once.

-- +goose Up
ALTER TABLE snapshots ADD COLUMN inputs TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE snapshots DROP COLUMN inputs;
