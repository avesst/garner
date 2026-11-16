# garner implementation plan

Status: MVP implemented, 2026-11-14. The design is in `design.md`; this file only says
in which order to build it and when each piece is done. Update it as slices
land.

## Project basics

- Repository: `github.com/avesst/garner` (Forgejo). May move to GitHub later.
- Go module path: `github.com/avesst/garner`. A move means one rewrite of the
  import paths; acceptable while garner is an application, not a library.
- Go: 1.26 (`go 1.26` in `go.mod`).
- CI: GitHub Actions. `ci.yml` runs `just check`, the build and the
  migrations on PostgreSQL; `e2e.yml` runs `just e2e`, except for pushes
  that change only documentation, `contrib/` or `scripts/`.
- Integration tests run real clients in containers (Docker locally and in CI).

## MVP step 1, as slices

Deb first, end to end, then rpm. Each slice ends in something that runs.

| # | Slice | Done when | Status |
|---|---|---|---|
| 1 | Skeleton: module, `cmd/garner`, CI running `go vet` and `go test` | CI passes on push | Done; CI not yet run |
| 2 | Configuration: load, validate, `config validate`, reload on `SIGHUP` (section 5) | Table tests cover valid and invalid files; an invalid reload keeps the old config | Done; `SIGHUP` wiring waits for `serve` (slice 6) |
| 3 | Database: SQLite, goose migrations, `flock` guard (sections 6, 12) | Migrations apply on SQLite; CI also applies them to PostgreSQL; a second process refuses to start | Done |
| 4 | Blob store, deb parsing, dpkg version comparison (sections 6, 7) | dpkg's own version test vectors pass; storing the same file twice is a no-op | Done; dpkg's vectors checked once, not committed (GPL), tests compare with `dpkg` instead |
| 5 | Signing: `init` generates a key; clearsign `InRelease` (section 10) | `sqv` and `gpgv` verify the output | Done; `gpgv` in unit tests, `sqv` through apt on Debian 13 and Ubuntu 26.04 |
| 6 | Publish deb: `Packages`, `Release`, hashed pool, by-hash with history, snapshot, `current` swap, landing page, client files, built-in server (sections 4, 9, 14) | `apt update && apt install` works in `debian:13` and `ubuntu:26.04` containers; an `apt update` with a publish forced between `InRelease` and `Packages` succeeds | Done (`just e2e`); without index history the race test fails with 404 |
| 7 | Snapshot list, rollback, GC (section 9) | A rollback is a new snapshot; GC removes only unreferenced blobs and respects `gc_grace` | Done |
| 8 | rpm: `createrepo_c`, re-signing with `rpmsign`, repodata history (sections 4, 9, 10) | `dnf install` works in `almalinux:10` and `fedora` containers with `gpgcheck` and `repo_gpgcheck` on | Done (`just e2e`, garner from its container image); dnf refuses a bad `repomd.xml.asc`; key rotation by restart re-signs the rpms |

## Later MVP steps

- Step 2: upload API and CLI, tokens, name ownership. Done.
- Step 3: GitHub sync. Done; tested against fake forges and once against
  a public GitHub repository (unauthenticated).
- Step 4: Forgejo/Gitea, GitLab and GitHub Enterprise Server clients. Done;
  tested against TLS test servers that mimic their APIs, not against real
  instances.
- Step 5: metrics, health endpoints, container image, systemd unit example.
  Done. A `.deb` of garner itself is built with `just deb`; an `.rpm` is
  not done (see the decisions below).

## Not verified yet

- No sync against a real Forgejo, GitLab or GitHub Enterprise Server, and
  none against GitHub with a token or an organization.
- The systemd unit example is checked with `systemd-analyze verify` only.

## Decisions to make when their slice arrives

| Decision | Needed by | Current recommendation |
|---|---|---|
| CLI library: standard `flag` or a framework | Slice 1 | Implemented with `flag` plus a helper that accepts flags after positional arguments |
| Snapshot retention default | Slice 7 | Implemented as a count (`storage.snapshot_retention`, 30); age and `snapshot keep` are open |
| rpm when its tools are missing | Slice 8 | Implemented: rpm off, and `serve` names the missing tools; rpm packages stay stored and come back when the tools do |
| One deb in several suites | Step 2 | `suite: [noble, jammy]` and repeated `?suite=` |
| Packaging garner (nfpm or GoReleaser; `.rpm` too?) | Step 5 | `.deb` built with `dpkg-deb` (`just deb`), tested in a Debian 13 systemd container; `.rpm` open |
| Default deployment, backup and restore, rpm `Signer` limit | Before release | Document; proxy serves `public/` |
| License | Before the first public release | Open |

## Dependencies to approve

Each is asked for before the slice that needs it.

- Approved and added: `go.yaml.in/yaml/v3` (slice 2), `modernc.org/sqlite`
  and `pressly/goose` (slice 3), `github.com/ulikunitz/xz` and
  `github.com/klauspost/compress` (slice 4; the xz module also writes
  `Packages.xz` in slice 6), `github.com/ProtonMail/go-crypto` (slice 5,
  which brings `cloudflare/circl`).
- Local implementations instead of dependencies: ULIDs, Prometheus metrics
  text format, forge API clients.
