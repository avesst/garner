-- Packages made of several blobs, mutable tags, and blobs a delivery in
-- progress has stored (design section 19). package_blobs lists the blobs a
-- package owns besides blob_sha256; deb and rpm packages have no rows.
-- Tags are labels on a package that can move; a snapshot records the tags
-- it served, so a rollback restores them. staged_blobs keeps blobs that no
-- package references yet, such as the layers of a push, from GC until
-- expires_at.

-- +goose Up
CREATE TABLE package_blobs (
    package_id TEXT NOT NULL REFERENCES packages (id),
    sha256     TEXT NOT NULL REFERENCES blobs (sha256),
    PRIMARY KEY (package_id, sha256)
);
CREATE INDEX package_blobs_by_blob ON package_blobs (sha256);

CREATE TABLE tags (
    repository_id TEXT NOT NULL REFERENCES repositories (id),
    format        TEXT NOT NULL,
    name          TEXT NOT NULL,
    tag           TEXT NOT NULL,
    package_id    TEXT NOT NULL REFERENCES packages (id),
    updated_at    TEXT NOT NULL,
    updated_by    TEXT NOT NULL,
    PRIMARY KEY (repository_id, format, name, tag)
);
CREATE INDEX tags_by_package ON tags (package_id);

CREATE TABLE snapshot_tags (
    snapshot_id TEXT NOT NULL REFERENCES snapshots (id) ON DELETE CASCADE,
    format      TEXT NOT NULL,
    name        TEXT NOT NULL,
    tag         TEXT NOT NULL,
    package_id  TEXT NOT NULL REFERENCES packages (id),
    PRIMARY KEY (snapshot_id, format, name, tag)
);
CREATE INDEX snapshot_tags_by_package ON snapshot_tags (package_id);

CREATE TABLE staged_blobs (
    sha256     TEXT PRIMARY KEY REFERENCES blobs (sha256),
    expires_at TEXT NOT NULL
);

-- +goose Down
DROP TABLE staged_blobs;
DROP TABLE snapshot_tags;
DROP TABLE tags;
DROP TABLE package_blobs;
