-- Release selection (design section 7) needs the publish time of the last
-- processed release, also after it has scrolled off the forge's first page.

-- +goose Up
ALTER TABLE seen_releases ADD COLUMN published_at TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE seen_releases DROP COLUMN published_at;
