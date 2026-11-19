-- Whether a held placement is held because its deliverer did not own the
-- name (design section 8). Such a placement does not decide its version
-- in the content check of section 7. Existing holds stay unmarked: the
-- reason was not recorded, so they keep deciding their version as before.

-- +goose Up
ALTER TABLE placements ADD COLUMN owner_conflict BOOLEAN NOT NULL DEFAULT FALSE;

-- +goose Down
ALTER TABLE placements DROP COLUMN owner_conflict;
