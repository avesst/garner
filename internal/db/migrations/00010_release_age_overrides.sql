-- Releases an operator let skip min_release_age (design section 7). An
-- override covers only a release published before it was made, so a
-- release deleted and re-created under the same tag waits again. Rows are
-- kept as the record of who skipped the wait.

-- +goose Up
CREATE TABLE release_age_overrides (
    source     TEXT NOT NULL,
    repo       TEXT NOT NULL,
    tag        TEXT NOT NULL,
    created_at TEXT NOT NULL,
    created_by TEXT NOT NULL,
    PRIMARY KEY (source, repo, tag)
);

-- +goose Down
DROP TABLE release_age_overrides;
