# garner

garner is an internal repository for packages and container images:
signed apt and dnf repositories and an OCI registry, filled from uploads,
pushes and releases synced from forges and registries. `README.md` covers what it does, `docs/design.md` is the spec,
and `docs/plan.md` tracks what is built and what is not yet verified.

## Checks

- `just check` is what CI runs. Run it before calling work done.
- Tests that compare against real tools (`dpkg`, `dpkg-deb`, `rpm`, `gpg`, `gpgv`)
  skip when the tool is missing. A skip is not a pass: run the package with
  `go test -v` and report which comparisons were skipped.
- `just e2e` runs real apt, dnf and container clients and needs Docker.
  Run it for changes to publishing, signing, repository layout or `/v2/`.
- PostgreSQL migration compatibility is checked only in CI
  (`scripts/check-migrations-postgres.sh` needs a server). Say when a
  migration change has not been through it.

## Design constraints

These come from `docs/design.md` sections 17 and 18, which explain them.

- The core is format-agnostic: deb and rpm sit behind one format interface,
  and code outside `internal/deb` and `internal/rpm` never branches on a
  format name. No table or column is named after a format.
- SQL stays portable to PostgreSQL: standard types and functions only.
  Schema changes go in a new numbered file in `internal/db/migrations`.
- Locks and publish requests go through the narrow database and publish
  interfaces, since multi-node support will back them with PostgreSQL.
  Coordination uses those interfaces in place of Go mutexes or channels.
- A publish ends in the single `rename(2)` of `public/current`.

## Keeping docs in step

A change to configuration keys updates, together:

- `internal/config` (types and validation),
- `docs/configuration.md` and the `docs/garner.yaml.5` man page,
- `contrib/garner.yaml` when the key belongs in a new installation's file.

A change to commands or flags updates `docs/garner.1`. When a slice lands
or an item gets verified, update `docs/plan.md`.

## Releases

A tag releases only with notes in `docs/releases/<tag>.md`, committed
before the tag; the release workflow publishes that file as the GitHub
release's notes. Write them for the people who run garner: what changed
for them since the previous release (features, behaviour and breaking
changes, in a line each), in prose and short lists, not a list of
commits. Draft them from `git log <previous tag>..` and `docs/plan.md`.
Keep each paragraph on one line: GitHub shows line breaks in release
notes as they are. `docs/releases/v0.1.0.md` shows the tone.

## Dependencies

Ask before adding a Go module. The project prefers a local implementation
for small needs (ULIDs, Prometheus text format and the forge clients are
local); approved modules are listed in `docs/plan.md`.
