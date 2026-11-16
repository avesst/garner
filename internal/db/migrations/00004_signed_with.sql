-- The fingerprint of the key the repository's packages were last signed
-- with (design section 10), so a start with a new key re-signs them.
-- Existing repositories get '' and are re-signed once.

-- +goose Up
ALTER TABLE repositories ADD COLUMN signed_with TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE repositories DROP COLUMN signed_with;
