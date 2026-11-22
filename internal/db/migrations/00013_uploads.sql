-- Upload sessions of a push in progress (design section 19). The data is
-- in uploads/<id> below storage.path; the row says how much of it is
-- written, so another node can continue the session (section 18).

-- +goose Up
CREATE TABLE uploads (
    id            TEXT PRIMARY KEY,
    repository_id TEXT NOT NULL REFERENCES repositories (id),
    format        TEXT NOT NULL,
    name          TEXT NOT NULL,
    size          BIGINT NOT NULL,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);

-- +goose Down
DROP TABLE uploads;
