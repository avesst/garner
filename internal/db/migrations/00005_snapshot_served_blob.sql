-- The blob each snapshot entry was served from, so GC keeps it while the
-- snapshot is retained even after a re-sign gives the package a new
-- signed blob (design section 9). Existing entries get the blob their
-- package serves now; for a package re-signed since that snapshot, the
-- old blob is already gone.

-- +goose Up
ALTER TABLE snapshot_packages ADD COLUMN served_sha256 TEXT NOT NULL DEFAULT '';
UPDATE snapshot_packages SET served_sha256 = (
    SELECT COALESCE(p.signed_blob_sha256, p.blob_sha256) FROM packages p
    WHERE p.id = snapshot_packages.package_id);

-- +goose Down
ALTER TABLE snapshot_packages DROP COLUMN served_sha256;
