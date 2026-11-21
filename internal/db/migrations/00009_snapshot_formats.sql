-- The formats each snapshot rendered, so a rollback changes only packages
-- of those formats: a format disabled at the time is absent from the
-- package set without its packages being gone (design section 9).
-- Existing snapshots get the formats of the packages they hold; one that
-- rendered a format without packages of it leaves that format alone.

-- +goose Up
CREATE TABLE snapshot_formats (
    snapshot_id TEXT NOT NULL REFERENCES snapshots (id) ON DELETE CASCADE,
    format      TEXT NOT NULL,
    PRIMARY KEY (snapshot_id, format)
);
INSERT INTO snapshot_formats (snapshot_id, format)
SELECT DISTINCT sp.snapshot_id, p.format
FROM snapshot_packages sp JOIN packages p ON p.id = sp.package_id;

-- +goose Down
DROP TABLE snapshot_formats;
