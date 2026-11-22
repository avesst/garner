-- Index for GC (design section 9). The primary key of snapshot_packages
-- leads with snapshot_id, so without this every package delete scans the
-- table for its foreign key, holding the write lock for as long as the
-- scans take. The table holds snapshot_retention copies of the active
-- set, so the scans grow with both. served_sha256 is left unindexed: its
-- lookups run one blob per short transaction, while an index on random
-- hashes would slow every publish's insert of the package set.

-- +goose Up
CREATE INDEX snapshot_packages_by_package ON snapshot_packages (package_id);

-- +goose Down
DROP INDEX snapshot_packages_by_package;
