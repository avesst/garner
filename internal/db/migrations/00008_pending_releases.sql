-- Releases whose last attempt failed with an error worth retrying (design
-- section 7). They are retried on every poll while newer releases of the
-- repository go ahead, and listings reach back to the oldest of them.

-- +goose Up
CREATE TABLE pending_releases (
    source       TEXT NOT NULL,
    repo         TEXT NOT NULL,
    release_id   TEXT NOT NULL,
    published_at TEXT NOT NULL,
    PRIMARY KEY (source, repo, release_id)
);

-- +goose Down
DROP TABLE pending_releases;
