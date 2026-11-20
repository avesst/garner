# garner implementation plan

Status: MVP implemented 2026-11-14; 0.1.0 candidate tested on a Debian 13
host 2026-11-20. The design is in `design.md`; this file only says
in which order to build it and when each piece is done. Update it as slices
land.

## Project basics

- Repository: `github.com/avesst/garner` on GitHub.
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
| 1 | Skeleton: module, `cmd/garner`, CI running `go vet` and `go test` | CI passes on push | Done |
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
  Done. `.deb` and `.rpm` packages of garner itself are built with
  GoReleaser (see the decisions below).

## Not verified yet

- No sync against a real Forgejo, GitLab or GitHub Enterprise Server, and
  none against GitHub with a token or an organization.
- On a real host, held packages and `approve`, a sync that adds a release,
  and a GC that deletes anything. Unit and e2e tests cover them.

## Verified on a test host

On 2026-11-20, a build of `3017cf8` was installed from its `.deb` over an
older build on Debian 13. It ran under the packaged systemd unit, behind
Caddy:

- The upgrade, restarts and reloads. The listener survives a restart, and
  `log.format` and `log.level` apply on reload.
- apt on the host and dnf in a Fedora container installed from the
  repository, dnf with `gpgcheck` and `repo_gpgcheck` on.
- Uploads, name ownership, token scopes and revocation, snapshot rollback,
  a pinned snapshot URL, publish, GC, and sync from a public GitHub
  repository.

## Next: minimum release age for sync (after 0.1.0)

Supply-chain protection: a synced release is published only once it is old
enough, so a compromised release has time to be noticed and pulled first.
Decided so far:

- Key `min_release_age`, a duration, in `defaults` and per source; a source
  value overrides the default, and `0s` (the default) turns it off. Same
  precedence as `retention`, `verify` and `approval`.
- Age is measured from the forge's `published_at`. Assets attached to an
  already processed release are never picked up (section 7), so editing an
  old release does not bypass it.
- A release that is too young stays pending and is not marked processed;
  later polls check it again, and newer releases are evaluated meanwhile.
- A source's first run takes the latest release that is old enough, not
  the latest release.
- Uploads are not affected.
- An explicit override for urgent fixes, for example
  `garner sync <source> --ignore-age <tag>`, so skipping the wait is a
  logged decision.
- Deferred releases show in `garner status` and as a metric.

## Decisions to make when their slice arrives

| Decision | Needed by | Current recommendation |
|---|---|---|
| CLI library: standard `flag` or a framework | Slice 1 | Implemented with `flag` plus a helper that accepts flags after positional arguments |
| Snapshot retention default | Slice 7 | Implemented as a count (`storage.snapshot_retention`, 30); age and `snapshot keep` are open |
| rpm when its tools are missing | Slice 8 | Changed before 0.1.0: a format is enabled by its config section. A configured rpm without its tools refuses to start and names them; removing the section keeps rpm packages stored, and they come back with it |
| One deb in several suites | Step 2 | `suite: [noble, jammy]` and repeated `?suite=` |
| Packaging garner (nfpm or GoReleaser; `.rpm` too?) | Step 5 | GoReleaser (nfpm inside): `.deb` and `.rpm` for amd64, installed and removed in Debian, Ubuntu, AlmaLinux and Fedora containers by `just test-packages` in CI; a semver tag makes a draft GitHub release. The `.deb` runs under systemd on the test host |
| Default deployment | 0.1.0 | Proxy serves `public/`; README "Put it behind a reverse proxy" |
| Backup and restore; rpm `Signer` limit (rpm re-signing needs an exportable key, so no HSM/KMS for rpm) | 1.0.0 | Document |
| `Valid-Until` in `InRelease`, so apt detects metadata frozen by an attacker | After 0.1.0 | Candidate. Needs a periodic re-sign and publish even when nothing changed, and an expiry window longer than that period. deb only; rpm has no equivalent |
| License | Before the first public release | 0BSD (`LICENSE`), also shipped in the packages |

## Dependencies to approve

Each is asked for before the slice that needs it.

- Approved and added: `go.yaml.in/yaml/v3` (slice 2), `modernc.org/sqlite`
  and `pressly/goose` (slice 3), `github.com/ulikunitz/xz` and
  `github.com/klauspost/compress` (slice 4; the xz module also writes
  `Packages.xz` in slice 6), `github.com/ProtonMail/go-crypto` (slice 5,
  which brings `cloudflare/circl`).
- Local implementations instead of dependencies: ULIDs, Prometheus metrics
  text format, forge API clients.
