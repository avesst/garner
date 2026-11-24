# garner implementation plan

Status: MVP implemented 2026-11-14; 0.1.0 released 2026-11-20. The design
is in `design.md`; this file only says in which order to build it and when
each piece is done. Update it as slices land.

## Project basics

- Repository: `github.com/avesst/garner` on GitHub.
- Go module path: `github.com/avesst/garner`. A move means one rewrite of the
  import paths; acceptable while garner is an application, not a library.
- Go: 1.26 (`go 1.26` in `go.mod`).
- CI: GitHub Actions. `ci.yml` runs `just check`, the build and the
  migrations on PostgreSQL; `e2e.yml` runs `just e2e`, except for pushes
  that change only documentation, `contrib/` or `scripts/`.
- Integration tests run real clients in containers (Docker locally and in CI).
- `just preview` (`cmd/garner-preview`) serves the UI with demo
  packages and a made-up status, and reloads it when `ui.html` changes.
  It is a development tool; releases build only `cmd/garner`.

## MVP step 1, as slices

Deb first, end to end, then rpm. Each slice ends in something that runs.

| # | Slice | Done when | Status |
|---|---|---|---|
| 1 | Skeleton: module, `cmd/garner`, CI running `go vet` and `go test` | CI passes on push | Done |
| 2 | Configuration: load, validate, `config validate`, reload on `SIGHUP` (section 5) | Table tests cover valid and invalid files; an invalid reload keeps the old config | Done; `SIGHUP` wiring waits for `serve` (slice 6) |
| 3 | Database: SQLite, goose migrations, `flock` guard (sections 6, 12) | Migrations apply on SQLite; CI also applies them to PostgreSQL; a second process refuses to start | Done |
| 4 | Blob store, deb parsing, dpkg version comparison (sections 6, 7) | dpkg's own version test vectors pass; storing the same file twice is a no-op | Done; dpkg's vectors checked once, not committed (GPL), tests compare with `dpkg` instead |
| 5 | Signing: `init` generates a key; clearsign `InRelease` (section 10) | `sqv` and `gpgv` verify the output | Done; `gpgv` in unit tests, `sqv` through apt on Debian 13 (Ubuntu 26.04's apt uses GnuPG's `gpgv`) |
| 6 | Publish deb: `Packages`, `Release`, hashed pool, by-hash with history, snapshot, `current` swap, UI, client files, built-in server (sections 4, 9, 14) | `apt update && apt install` works in `debian:13` and `ubuntu:26.04` containers; an `apt update` with a publish forced between `InRelease` and `Packages` succeeds | Done (`just e2e`); without index history the race test fails with 404 |
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

On 2026-11-22, with `e65e25b`, 539 real packages (Debian 12, 13 and sid,
Ubuntu 22.04 and 24.04, Fedora 44, AlmaLinux 9 and 10, GitHub releases;
1.33 GB) were uploaded one by one into their own suites and releases on a
container with 2 cores and 1 GiB. 536 were accepted; the rest were an
unconfigured architecture and a name owned by a source. garner peaked at
121 MB RSS and published 546 packages in 1.8 s. In containers of each
distribution, apt and dnf (`gpgcheck` and `repo_gpgcheck` on) downloaded
every indexed package and checked it against the indexes and, for rpm,
the key. Packages were not installed. A build of the verify branch then
reported no problems on that tree.

## Repository verification (after 0.1.0)

Done: `garner verify` and `POST /api/v1/verify` (design section 9,
Verification). Unit tests break each checked property; every e2e test
ends with a verify; on the test host the corpus above verifies in under a
second, and a pool file moved aside is reported. It found that a format
without packages had dangling pool links (fixed). Not done: re-hashing
blobs (`--deep`), for which a background job would be needed.

## Lifecycle e2e tests (after 0.1.0)

Done: `TestAptLifecycle` (Debian 13, Ubuntu 26.04) and `TestDnfLifecycle`
(AlmaLinux 10, Fedora 44) keep one client container through a
repository's life, with garner verify after each step. apt: install,
upgrade, an older version on purpose, removal, rollback, a second suite,
i386 multiarch, and a GC that deletes blobs and pool files. dnf: install,
upgrade, removal, rollback. Since 2026-11-22 the e2e client tests run on
every supported release: Debian 12 and 13, Ubuntu 22.04, 24.04 and 26.04,
AlmaLinux 9 and 10, Rocky Linux 10 and Fedora 44. Not covered yet: crash
and concurrency tests.

## Minimum release age for sync (after 0.1.0)

Done; unit-tested against fake forges, not in `just e2e`. On 2026-11-22
a build of `86b6b92` on the test host ran a gated source (`cli/cli`,
168h, unauthenticated GitHub): the first run took the newest old-enough
release and deferred the newer one, shown in status, the metric and the
log once; the listing's ETag was not kept while it waited; the override
was refused without admin or publish, for a processed tag, a wrong repo
and an ungated source, then let the release through, recorded with the
token's name; apt saw the new version; a reload that changed the age
restarted only that source. Not seen on a real forge yet: a release
ageing past the gate on its own, a pulled release, lookups by ID, and
Forgejo or GitLab. Supply-chain protection: a synced release is published only
once it is old enough, so a compromised release has time to be noticed and
pulled first.

- Key `min_release_age`, a duration, in `defaults` and per source; a source
  value overrides the default, and `0s` (the default) turns it off. Same
  precedence as `retention`, `verify` and `approval`.
- Age is measured from the forge's publish time, or from the newest
  candidate asset's upload where the forge dates assets (GitHub: later of
  `created_at` and `updated_at`; Forgejo: `created_at`). On GitLab the
  publish time is the later of `created_at` and `released_at`, since
  `released_at` can be backdated; the listing keeps GitLab's `released_at`
  order, so upcoming releases are found, and a backdated release far back
  may not be listed (nothing is published early). A
  release without a publish time waits until an override.
- A release that is too young is deferred: recorded as pending, so a newer
  release processed meanwhile does not hide it, and the listing's ETag is
  not stored, so later polls check it again.
- A source's first run takes the latest release that is old enough; newer
  ones follow as they age, so a new gated source can publish several
  recent versions, where an ungated one takes only the newest.
- The wait for late assets stays on where assets are dated: late assets
  wait for their own age. On GitLab (undated links) an old-enough release
  is processed as it is, except one let through by an override.
- Uploads are not affected.
- `garner sync <source> --ignore-age <tag> [--repo <repo>]` lets one
  release through. It needs `publish` and `admin`, is logged, and is kept
  in `release_age_overrides` with the token's name. It covers only a
  release and dated assets uploaded before the override, so one
  re-created under the same tag, or an asset uploaded later, waits again.
  `--repo` must match the source's cached listing, ignoring case. A tag
  already processed is refused with 409; an override for a release the
  source never processes is logged as having no effect.
- A pending release the listing does not hold is fetched by ID; one that
  is gone (pulled) or became a draft is dropped with a warning, so polls
  stop paging back to it. While any is pending the listing is fetched
  without its ETag.
- GitLab release IDs are the tag plus `created_at`, so a release
  re-created under the same tag is new. Releases processed under the old
  ID (the bare tag) still count as processed, and one pending under it is
  kept pending under its new ID.
- Open, GitLab only: a release processed before the ID change and later
  re-created under its tag still counts as processed through its former
  ID. Fix when needed: count a former-ID match only if that record's
  `published_at` is not before the release's `created_at`.
- Releases without package assets are not deferred, and a pending one
  whose package assets were removed is dropped, so its repository's
  listing can use its ETag again.
- A failed lookup of a pending release holds up only that release.
- An override on a source without `min_release_age` is refused (409).
- Known limit, older than this slice: listings stop paging at the first
  release published at or before `since`, but forges sort by creation.
  A release created long before it is published (a draft kept for weeks)
  can sit past that page and is never seen, gate or not. Pending releases
  are not affected: they are fetched by ID.
- Known limit: on Forgejo a rename does not move an asset's time, so an
  old non-package file renamed to `.deb` on an unprocessed release counts
  as old.
- Deferred releases show in `garner status` and as
  `garner_sync_releases_deferred{source}`.
- Open, for multiple nodes (section 18): the exception that lets
  `--ignore-age` through for a processed tag re-created upstream reads the
  syncer's in-memory deferred list. It is empty after a restart until the
  source's first poll (the 409 message says to run `garner sync` first)
  and is not shared between nodes. Fix with the multi-node work: a `tag`
  column in `pending_releases`, and the decision made from the database.

## OCI images (after 0.1.0)

Designed in design section 19 and built in `internal/oci`.

| # | Slice | Done when | Status |
|---|---|---|---|
| O1 | Core: `package_blobs`, `tags`, `snapshot_tags`, `staged_blobs`, multi-blob pool links and GC, `Record` ingest; one glob matcher for `owns`, reservations by source kind | Existing tests pass; GC keeps blobs shared by packages and staged blobs until they expire; a rollback restores tags | Done (unit tests in `internal/oci`, `internal/glob`, `internal/ingest`) |
| O2 | Pull: render manifests, tag lists and blob links per snapshot; `/v2/` in the built-in server; `oci:` config section | docker, podman, skopeo and crane pull by tag and digest, single- and multi-arch; `garner verify` checks the tree | Done (`TestOCIClients`) |
| O3 | Push: upload sessions, manifests, token auth on `/v2/`, ownership, `DELETE` | `docker push` and a multi-arch `docker buildx imagetools create` work and can be pulled after publish; `docker login` rejects a bad token; anonymous pulls work; another owner's push is refused | Done (`TestOCIClients`, `TestOCIPushRefusedForOtherOwner`, unit tests for chunks, resume and digests) |
| O4 | Registry sync: local client, `registries:`, image sources, tag globs, `min_release_age` from first sight | Unit tests against a fake registry with Bearer auth on another host and a CDN redirect that must not get credentials | Done; also against ghcr.io, Docker Hub and registry.gitlab.com (below) |
| O5 | UI, `garner.1`, `garner.yaml.5`, `docs/configuration.md`, `contrib/garner.yaml`, README proxy examples | `just check` and `just e2e` pass | Done |

Settled while building:

- `GET /v2/` answers `401` with a `Bearer` challenge and `/v2/token`
  hands out anonymous tokens. It first answered with a `Basic`
  challenge, which Docker's classic image store (the CI runners') turns
  into `no basic auth credentials` on anonymous pulls; Docker 29 with the
  containerd store pulled anyway, which is why local runs passed.
  `TestOCIClients` passes against Docker 29.10 with the classic store in
  dind.
- The reverse proxy forwards all of `/v2/` to garner. The snapshot tree
  could be served statically, but no proxy recipe for it is shipped or
  tested.
- `/v2/` serves only published image blobs, plus `HEAD` on blobs a push
  staged; held packages' blobs are not reachable.

On 2026-11-22 a build of the OCI work was installed from its `.deb` on
the test host (Debian 13, systemd, behind Caddy with TLS) over the
corpus of 536 packages:

- Registry sources synced anonymously from ghcr.io
  (`containerd/busybox`), Docker Hub (`library/alpine`, with
  `auth_hosts: [auth.docker.io]`) and registry.gitlab.com
  (`gitlab-org/release-cli`, with `auth_hosts: [gitlab.com]`) on their
  first poll; docker on another host pulled and ran them.
- Through Caddy: `docker login` refused a bad token; `docker push` and
  `docker buildx imagetools create` (a multi-arch index, its blobs mounted
  from another name) worked; a push to a source-owned name was refused;
  anonymous pulls, an arm64 pull and the tag list worked; the UI
  listed the images.
- Tag `DELETE` (admin only), `garner remove` of an image name with `/`,
  publish, GC and `garner verify` (no problems, 550 packages), and apt on
  the host, all after the upgrade.

Not verified yet:

- Static serving of the `/v2/` tree by a proxy.
- Registry sync with credentials (all three tests were anonymous) and
  `min_release_age` on a real registry.
- The OCI 1.1 referrers API is not implemented; cosign's tag scheme works
  as ordinary tags but has not been tried with cosign.
- Not tried: `docker push` through nginx.
- Single-node limits, for the multi-node work (design section 18): a
  registry's `max_concurrent` is an in-process limit, and the client
  cache is guarded by a Go mutex.

## UI by ecosystem (after 0.1.0)

Done; design section 14. One tab per ecosystem (formats grouped by
namespace) with a summary, then every package or image of the tab,
paged 25 at a time with a filter. Separate `packages.html` and
`images.html` pages were tried first and dropped for the in-tab lists.
`TestUIByEcosystem` renders deb, rpm and images and checks each
list stays in its tab; `TestUIManyImages` lists 501 images in the
page. Paging and filtering were checked in headless Chromium (25 of 501
rows shown); not checked with screen readers. `index.html` grows with
the number of names, about 150 KB for 500 images.

## First run past releases without candidates (after 0.2.0)

Fixed; design section 7, Release selection. A source's first run took the
newest release by publish time, and when its assets matched no rule it
waited for them, so the source never started. This hit
`open-telemetry/opentelemetry-collector-releases`, which publishes
`cmd/opampsupervisor/vX` seconds after `vX`. The first run now takes the
newest release with candidate assets. Unit-tested
(`TestFirstRunSkipsNewerReleaseWithoutCandidates`); not yet run against
that repository.

## Decisions to make when their slice arrives

| Decision | Needed by | Current recommendation |
|---|---|---|
| CLI library: standard `flag` or a framework | Slice 1 | Implemented with `flag` plus a helper that accepts flags after positional arguments |
| Snapshot retention default | Slice 7 | Implemented as a count (`storage.snapshot_retention`, 30); age and `snapshot keep` are open |
| rpm when its tools are missing | Slice 8 | Changed before 0.1.0: a format is enabled by its config section. A configured rpm without its tools refuses to start and names them; removing the section keeps rpm packages stored, and they come back with it |
| One deb in several suites | Step 2 | `suite: [noble, jammy]` and repeated `?suite=` |
| Packaging garner (nfpm or GoReleaser; `.rpm` too?) | Step 5 | GoReleaser (nfpm inside): `.deb` and `.rpm` for amd64, installed and removed in Debian, Ubuntu, AlmaLinux and Fedora containers by `just test-packages` in CI; a semver tag publishes a GitHub release (a pre-release for `-rc` and the like). The `.deb` runs under systemd on the test host |
| Default deployment | 0.1.0 | Proxy serves `public/`; README "Put it behind a reverse proxy" |
| Backup and restore; rpm `Signer` limit (rpm re-signing needs an exportable key, so no HSM/KMS for rpm) | 1.0.0 | Document |
| `Valid-Until` in `InRelease`, so apt detects metadata frozen by an attacker | After 0.1.0 | Candidate. Needs a periodic re-sign and publish even when nothing changed, and an expiry window longer than that period. deb only; rpm has no equivalent |
| i686 rpms (multilib): published as their own `rpm/<release>/i686` repository, which x86_64 clients do not read, while Fedora and EL ship multilib packages in the x86_64 repository | Before relying on i686 | Done: i686 packages are listed in the x86_64 repository and i686 needs x86_64 configured; dnf installs both architectures side by side in AlmaLinux 10 and Fedora 44 (`just e2e`). deb is unchanged: apt with `dpkg --add-architecture i386` installs a `Multi-Arch: same` package for amd64 and i386 side by side (`TestAptLifecycle`) |
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
