#!/bin/sh
# Apply every migration's "Up" section to PostgreSQL in one transaction, to
# catch SQLite-only SQL before multi-node support needs it (design section 18).
# Connection settings come from the standard PG* environment variables.
set -eu

dir=$(dirname "$0")/../internal/db/migrations

for f in "$dir"/*.sql; do
	echo "-- $f"
	# Print lines after "-- +goose Up" until "-- +goose Down".
	awk '/^-- \+goose Down/ { exit } up { print } /^-- \+goose Up/ { up = 1 }' "$f"
done | psql --no-psqlrc --quiet -v ON_ERROR_STOP=1 --single-transaction

echo "migrations apply cleanly on PostgreSQL"
