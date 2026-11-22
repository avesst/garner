-- Tag state of registry sources (design section 19). source_tags is the
-- upstream digest each tag was last applied from, so a tag moves only
-- when upstream moves it, and a rollback or a deleted tag holds until
-- then. source_tag_candidates are the digests seen under a tag, with when
-- garner first saw each, so min_release_age can pass the newest one that
-- is old enough while upstream keeps moving the tag. Both are kept per
-- local name, so a source that changes its name syncs anew.

-- +goose Up
CREATE TABLE source_tags (
    source     TEXT NOT NULL,
    repo       TEXT NOT NULL,
    name       TEXT NOT NULL,
    tag        TEXT NOT NULL,
    digest     TEXT NOT NULL,
    applied_at TEXT NOT NULL,
    PRIMARY KEY (source, repo, name, tag)
);

CREATE TABLE source_tag_candidates (
    source     TEXT NOT NULL,
    repo       TEXT NOT NULL,
    name       TEXT NOT NULL,
    tag        TEXT NOT NULL,
    digest     TEXT NOT NULL,
    first_seen TEXT NOT NULL,
    PRIMARY KEY (source, repo, name, tag, digest)
);

-- +goose Down
DROP TABLE source_tag_candidates;
DROP TABLE source_tags;
