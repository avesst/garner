-- When a snapshot stopped being current, so GC measures gc_grace from it
-- (design section 9). Existing snapshots get the creation time of the
-- next newer one, which GC used before; the current one keeps ''.

-- +goose Up
ALTER TABLE snapshots ADD COLUMN replaced_at TEXT NOT NULL DEFAULT '';
UPDATE snapshots SET replaced_at = COALESCE((
    SELECT MIN(n.created_at) FROM snapshots n
    WHERE n.created_at > snapshots.created_at), '')
WHERE NOT is_current;

-- +goose Down
ALTER TABLE snapshots DROP COLUMN replaced_at;
