# garner design

Status: MVP implemented (see `plan.md`). Decisions recorded 2026-10-13,
updated 2026-11-14 with choices made during implementation.

## 1. Purpose

garner serves an internal Linux package repository for `.deb` and `.rpm`
packages. It accepts packages from two places:

- uploads, typically from CI pipelines;
- release assets synced from GitHub, GitHub Enterprise Server, GitLab and
  Forgejo/Gitea, from single repositories or from whole organizations/groups.

The MVP is one process with SQLite and local disk, for a homelab and for an
internal enterprise repository behind a reverse proxy. Multiple nodes on
PostgreSQL come later (section 18).

### Goals

- One static Go binary; minimal setup for a single-node install.
- Clients trust exactly one key: the repository's. garner does the upstream
  verification.
- Declarative configuration that fits a GitOps workflow.
- Atomic publishing: clients never see a half-written repository.
- Later: a path from SQLite and one node to PostgreSQL and multiple nodes
  without reinstalling (section 18). The MVP keeps this possible but does not
  build it.

### Non-goals

- Mirroring upstream distributions (Pulp and aptly cover this).
- Building packages from source.
- Formats other than deb and rpm in the MVP. More formats are expected later;
  section 17 lists what the MVP must not rule out.
- Per-client filtering of repository contents.

## 2. Terminology

| Term | Meaning |
|---|---|
| Distribution | A deb suite (`stable`, `noble`) or an rpm release (`stable`, `el9`). Stored as one concept with a format. |
| Source | A configured sync origin: one forge repository, or one organization/group. |
| Forge | A configured forge instance (`github`, `forgejo-intern`, ...). |
| Owner | The identity that owns a package name: a token's `owner` label or `source:<name>`. |
| Blob | An immutable file in the content-addressed store, keyed by sha256. |
| Snapshot | An immutable, signed set of repository metadata produced by one publish. |

## 3. Architecture

```
            uploads (API/CLI)        forge sync
                    \                 /
                     v               v
                 +-----------------------+
                 |        ingest         |  parse, verify, ownership,
                 +-----------------------+  conflict checks, rpm re-sign
                     |               |
                     v               v
              blob store (FS)     database (SQLite)
                     \               /
                      v             v
                 +-----------------------+
                 |        publish        |  render + sign metadata,
                 +-----------------------+  write snapshot, atomic swap
                            |
                            v
                 public tree on disk  --->  built-in HTTP server
                                       --->  or nginx / proxy serving the
                                             same directory
```

### Process model

- One process (`garner serve`) does everything: it serves the repository,
  the landing page and the API, accepts uploads, and runs sync, publish,
  retention and garbage collection.
- At startup it takes an `flock` on `<db>.lock` and refuses to start if
  another process holds it.
- Ingest records a package in the database and requests a publish. Publish
  requests are debounced (default 10 s) so a burst of uploads yields one
  publish.
- `public/` is plain files. When the reverse proxy serves it directly from
  disk (section 4), apt and dnf clients keep working while garner is stopped
  or restarting; only uploads and sync pause. With the built-in server, reads
  stop too.

Multiple nodes and separate roles are later extensions (section 18).

## 4. Repository layout

### Public URLs

```
/                         landing page (static per snapshot, live status)
/key.asc                  public key(s), ASCII-armored
/key.gpg                  public key(s), binary keyring (for apt Signed-By)
/garner.sources           generated deb822 client file
/garner.repo              generated dnf client file
/deb/dists/<suite>/...    apt metadata (current snapshot)
/deb/pool/...             deb package files
/rpm/<release>/<basearch>/repodata/...   rpm metadata (current snapshot)
/rpm/<release>/<basearch>/pool/...       rpm package files
/snapshots/<id>/deb/...   a specific snapshot, for pinning
/snapshots/<id>/rpm/...
/healthz /readyz /metrics
/api/v1/...
```

The file name prefix of the generated client files (`garner`) comes from
`server.client_name` in the configuration.

### deb

- One apt archive under `/deb`.
- Suites come from configuration; the default is only `stable`.
- One component, `main`, in the MVP.
- `dists/<suite>/InRelease`, `Release`, `Release.gpg`,
  `main/binary-<arch>/Packages{,.gz,.xz}`, with `Acquire-By-Hash: yes` and
  `by-hash/SHA256/` directories. Each snapshot's `by-hash/` also holds the
  index files of recent earlier snapshots (section 9), so a client that read
  the previous `InRelease` can finish its update after a publish.
- `Architectures:` in `Release` lists the configured architectures, not just
  the ones present. Packages with `Architecture: all` are listed in every
  `binary-<arch>/Packages`.
- Pool path: `pool/main/<prefix>/<name>/<hash>/<file>`, where `<prefix>` is
  the first letter of the name, or the first four characters for names
  starting with `lib`; `<hash>` is the first 16 hex characters of the served
  file's sha256; and `<file>` is `<name>_<version>_<arch>.deb` built from the
  control fields (without epoch), not the uploaded file name.
- The hash in the path lets two suites carry different files with the same
  name, version and architecture, as per-distribution builds from forges
  often do. Identical files share one path. If a path already holds a
  different file (a 64-bit prefix collision), ingest rejects the new file.
- Within one suite, name, version and architecture stay unique (section 7). A
  client that enables two suites carrying different files for the same
  version will see apt treat them as one package; clients are expected to
  enable the suite for their own OS release.

Client example (the landing page renders this):

```
Types: deb
URIs: https://repo.example.com/deb
Suites: stable
Components: main
Architectures: amd64
Signed-By: /usr/share/keyrings/garner.gpg
```

### rpm

- One repository per release and architecture:
  `/rpm/<release>/<basearch>/repodata/`.
- Releases come from configuration; the default is only `stable`.
- `noarch` packages are included in each architecture's repodata; the file is
  stored once.
- `location href` in `primary.xml` is `pool/<prefix>/<hash>/<file>`, with
  `<hash>` as for deb, taken from the re-signed file that is served. Each
  `/rpm/<release>/<basearch>/pool` resolves to the shared rpm pool, so dnf
  never needs `..` in a path.
- `repomd.xml` is signed (`repomd.xml.asc`). Packages are re-signed with the
  repository key (section 9).
- Metadata file names carry their hash (`<sha256>-primary.xml.zst`). Each
  snapshot's `repodata/` also holds the metadata files of recent earlier
  snapshots (section 9), so a client that read the previous `repomd.xml` can
  finish its update after a publish.

Client example:

```
[garner]
name=garner
baseurl=https://repo.example.com/rpm/stable/$basearch/
enabled=1
gpgcheck=1
repo_gpgcheck=1
gpgkey=https://repo.example.com/key.asc
```

### On-disk layout

```
/var/lib/garner/
  garner.db, garner.db.lock          SQLite only; must be on local disk
  blobs/sha256/ab/abcdef...           content-addressed originals and
                                      re-signed rpm variants
  pool/deb/main/...                   hardlinks into blobs/
  pool/rpm/...                        hardlinks into blobs/
  snapshots/<id>/                     metadata only:
    deb/dists/...
    rpm/<rel>/<arch>/repodata/...
    index.html, key.asc, key.gpg, garner.sources, garner.repo
  public/                             the served tree
    current   -> ../snapshots/<id>    the only link a publish changes
    deb/dists -> ../current/deb/dists
    deb/pool  -> ../../pool/deb
    rpm/<rel>/<arch>/repodata -> ../../../current/rpm/<rel>/<arch>/repodata
    rpm/<rel>/<arch>/pool     -> ../../../../pool/rpm
    snapshots -> ../snapshots
    index.html -> current/index.html  (likewise key.asc, key.gpg and the
                                      client files)
```

Pool entries are hardlinks to blobs. If hardlinks are not possible, garner
falls back to copying.

`public/current` is the only symlink whose target changes. Every other link
in `public/` has a fixed target through `current`, so one `rename(2)` switches
deb, rpm, the landing page, the keys and the client files together, and a
client never sees two snapshots at once. A publish creates fixed links that
are missing (a new suite, release or architecture) and removes those no
longer configured; it never retargets them.

The landing page, keys and client files belong to the snapshot so that they
change with it. During key rotation (section 10), the published keyring then
always matches the signatures on the current metadata.

`public/` can be served by the built-in server or directly from disk by the
reverse proxy. Which proxy is not fixed; it must follow symlinks (in nginx,
`disable_symlinks off`, the default). The pool is shared and append-only between GC runs.
Snapshots contain only metadata, plus a fixed `pool` link per format (and
per rpm release and architecture) so clients pinned to `/snapshots/<id>/`
can download packages.

The built-in server ignores `If-Modified-Since` for metadata outside
`by-hash/` and the pool: `Last-Modified` has one-second resolution, so a
client could get `304` for an `InRelease` changed by a second publish
within the same second. A proxy serving `public/` from disk has the same
limit; publishes are debounced (section 3), which makes it rare.

### Supported clients

garner is tested against the current stable release of each client
distribution: Debian, Ubuntu LTS, RHEL and its rebuilds (AlmaLinux, Rocky),
and Fedora. As of 2026-11 that is Debian 13, Ubuntu 26.04 LTS and EL 10.
Older releases may work but are not tested in the MVP; testing the previous
release too (Ubuntu 24.04 LTS, EL 9) is planned for later. All of these verify signatures
with Sequoia-based tooling and reject SHA-1; garner signs with RSA 4096 and
SHA-256 or stronger.

## 5. Configuration

A single YAML file holds human decisions. The database holds state.

- Default path: `/etc/garner/garner.yaml`; override with `--config`.
- Secrets are never inline. Use `*_file` (Kubernetes secrets, systemd
  `LoadCredential=`) or `*_env`.
- `garner config validate <file>` validates offline and can run in CI.
- Reload with `SIGHUP` or `garner reload`. An invalid file is rejected,
  logged, and the last good configuration stays active.
- The configuration hash (and the git commit, if the file is inside a git
  working tree) is shown on the landing page and in `/api/v1/status`.
- The CLI never modifies anything the file defines.

### Example

```yaml
server:
  listen: ":8080"
  public_url: https://repo.example.com
  client_name: garner
  trusted_proxies: ["10.0.0.0/8"]
  allow_insecure_auth: false
  max_upload_size: 2GiB

metrics:
  listen: ""                  # empty: serve /metrics on server.listen

log:
  format: json                # json | text
  level: info

database:
  url: sqlite:///var/lib/garner/garner.db

storage:
  path: /var/lib/garner
  gc_grace: 10m
  index_history: 1h           # carry over index files from this window
  snapshot_retention: 30      # snapshots kept, newest first

signing:
  keys:                       # first key signs; all are published
    - key_file: /etc/garner/keys/2026.asc
      passphrase_file: ""

deb:
  architectures: [amd64, arm64]
  suites: [stable, noble]

rpm:
  architectures: [x86_64, aarch64]
  releases: [stable, el9]

defaults:
  retention: 3
  poll_interval: 1h
  verify: opportunistic       # opportunistic | required
  approval: auto              # auto | manual
  prereleases: false

forges:
  - name: github
    type: github
    token_env: GITHUB_TOKEN
  - name: forgejo-intern
    type: forgejo              # gitea is an alias
    url: https://git.example.com
    token_file: /run/secrets/forgejo
    ca_file: /etc/ssl/certs/internal-ca.pem
    max_concurrent: 4
  - name: gitlab-intern
    type: gitlab
    url: https://gitlab.example.com
    token_file: /run/secrets/gitlab
    proxy: http://proxy.example.com:3128

sources:
  - name: tool
    forge: forgejo-intern
    repo: platform/tool
    owns: ["tool", "tool-*"]
    assets:
      - match: "*_ubuntu24.04.deb"
        suite: noble
      - match: "*.el9.*.rpm"
        release: el9
      - match: "*"
        suite: stable
        release: stable
    exclude: ["*-musl*"]

  - name: platform-group
    forge: gitlab-intern
    group: platform           # org: for github/forgejo
    include: ["*"]
    exclude: ["legacy-*"]
    archived: false
    forks: false
    recursive: true           # GitLab subgroups
    poll_interval: 30m
    retention: 5
```

Every suite or release referenced in `assets` must exist in `deb.suites` or
`rpm.releases`. Validation rejects unknown ones.

## 6. Data model

The schema is portable across SQLite and PostgreSQL: no `JSONB`, no arrays, no
dialect-specific functions in the core schema. Structured fields are `TEXT`
holding JSON, read only by the application. Migrations use `pressly/goose`.
The MVP supports SQLite only. To keep the schema portable, CI also applies all
migrations to PostgreSQL; the test suite itself runs on SQLite.

| Table | Key columns | Notes |
|---|---|---|
| `repositories` | `id`, `name` | One row (`default`) in the MVP. Every other table references it, so named repositories can be added without a data migration. |
| `blobs` | `sha256` PK, `size`, `created_at` | Originals and re-signed variants. |
| `packages` | `id`, `repository_id`, `format`, `name`, `epoch`, `version`, `release`, `arch`, `blob_sha256`, `signed_blob_sha256`, `metadata` (TEXT/JSON), `origin` (`upload`/`source`), `origin_ref`, `verify_status`, `pinned`, `created_at` | Unique on (`repository_id`, `format`, `name`, `epoch`, `version`, `release`, `arch`, `distribution`) through `placements`. |
| `placements` | `package_id`, `distribution`, `state` (`active`/`held`/`removed`), `created_at` | Where a package is published. |
| `name_owners` | `repository_id`, `namespace`, `name` PK, `owner`, `source` (`first`/`config`/`manual`), `assigned_at` | One owner per name within a namespace. deb and rpm share the namespace `os`, the only one in the MVP (section 17). |
| `conflicts` | `id`, `kind`, `package_ref`, `detail`, `created_at`, `resolved_at` | Ownership, content and asset-mapping conflicts. |
| `source_state` | `source` PK, `last_run`, `last_success`, `last_error`, `etags` (TEXT/JSON) | |
| `source_repos` | `source`, `repo`, `etag`, `last_seen` | Cached org/group listing. |
| `seen_releases` | `source`, `repo`, `tag`, `release_id`, `processed_at` | Prevents reprocessing. |
| `snapshots` | `id`, `created_at`, `config_hash`, `is_current` | |
| `snapshot_packages` | `snapshot_id`, `package_id`, `distribution` | Drives GC reachability. |
| `tokens` | `id`, `name`, `owner`, `scopes`, `hash`, `created_at`, `last_used_at`, `revoked_at` | sha256 of a 256-bit random secret. |
| `events` | `id`, `at`, `actor`, `action`, `detail` | Audit log. Later, not in the MVP (section 15). |

Versions are compared with dpkg and rpmvercmp semantics, implemented in Go.
The database never sorts versions.

## 7. Ingest

### Common pipeline

1. Store the file as a blob (fsync, then rename into place).
2. Detect the format from content, not the file name. Parse name, version and
   architecture from package metadata.
3. Reject architectures that are not configured (logged; for sync, also shown
   in status).
4. Check name ownership (section 8).
5. Check content conflicts: the same (format, name, epoch, version, release,
   arch, distribution) with a different sha256 is rejected. The same sha256
   is a no-op. The same package with different content in another
   distribution is allowed; the pool path keeps the files apart (section 4).
6. rpm: produce a re-signed variant (section 10). Signing at ingest reports
   a signing failure to the uploader instead of failing a later publish.
7. Record the package and its placements; request a publish.

### Upload

```
PUT /api/v1/upload?suite=stable          # deb
PUT /api/v1/upload?release=el9           # rpm
Authorization: Basic <name>:<token>
Body: the package file
```

- Without `suite`/`release`, the target is `stable`.
- Requires the `upload` scope.
- `200` on success or an identical re-upload. `409` on a content conflict or
  when the name is owned by someone else. `403` on a missing scope.
- `400` when the suite or release is not configured; the response lists the
  configured ones.
- `413` when the file exceeds `server.max_upload_size` (default 2 GiB).
- Uploads are published immediately (`approval` does not apply).
- `garner upload [--suite S] [--release R] FILES...` wraps the API.

```
curl -fsS -u ci:$GARNER_TOKEN -T tool_1.4.0_amd64.deb \
  "https://repo.example.com/api/v1/upload?suite=stable"
```

### Forge sync

**Polling.** Each source has a `poll_interval`, with ±10 % jitter. Requests
use `If-None-Match` with stored ETags. `garner sync <source>` triggers an
immediate run. Webhooks come later and will only trigger a poll; the payload is
not trusted.

**Forge clients.**

| Type | Releases API | Asset download |
|---|---|---|
| `github` | `/repos/{o}/{r}/releases` | the API asset `url` with `Accept: application/octet-stream` (works for private repositories); `digest` field when present |
| `forgejo`/`gitea` | `/api/v1/repos/{o}/{r}/releases` | `browser_download_url` |
| `gitlab` | `/api/v4/projects/{id}/releases` | `assets.links[]` (often Generic Package Registry or external URLs) |

- Credentials are sent only to the forge's own host (same scheme, host and
  port as `url`). Links to other hosts are fetched without credentials; if
  that fails, the asset fails with a clear error.
- TLS verification cannot be disabled. A custom CA is set with `ca_file`.
- Proxy: `HTTPS_PROXY`/`NO_PROXY` by default; `proxy` per forge overrides.
- At most `max_concurrent` requests per forge (default 4). garner honours
  `Retry-After` and rate-limit headers, and exports the remaining quota as a
  metric.

**Organization/group sources.**

- The repo list is fetched on every poll; new repos are included
  automatically.
- `include`/`exclude` are globs on repo names (`exclude` also applies to
  asset names, see below). Archived repos and forks are
  skipped by default. GitLab subgroups are walked recursively by default.
- Repos without matching assets are ignored but listed in status.

**Release selection.**

- Drafts are never used. Pre-releases are skipped unless `prereleases: true`.
- First run of a source (or a newly discovered repo): only the latest
  published release.
- Later runs: every release published after the last processed one, ordered
  by the forge's publish timestamp (`published_at` or equivalent). Tags are
  not compared as versions, because many are not (`nightly`,
  `2024-05-release`). `seen_releases` keeps a release from being processed
  twice, also when its timestamp is edited.
- If upstream deletes a release, the packages stay; retention handles them.

**Asset selection.**

- Candidates: `*.deb` and `*.rpm`, excluding `*.src.rpm`, `*-dbgsym*`,
  `*-debuginfo*`, plus the source's `exclude` list.
- `assets` rules are evaluated in order; the first rule that matches the
  asset name and sets a target for the asset's format wins: `suite` for deb,
  `release` for rpm. A `*` rule with only `suite` therefore does not
  swallow rpm assets. With no rules, the target is `stable`; with rules but
  none applying, the asset is skipped.
- If two assets in one release map to the same (name, version, arch,
  distribution), both are skipped and a conflict is recorded. garner never
  guesses.

**Verification.**

- If the forge exposes an asset digest, or the release contains a checksum file
  (`checksums.txt`, `*_checksums.txt` as GoReleaser names it, `SHA256SUMS`,
  `*.sha256`), the asset must match it. A
  mismatch rejects the asset.
- `verify: opportunistic` accepts assets without a checksum and marks them
  `unverified`. `verify: required` rejects them.
- Later: a pinned upstream GPG fingerprint (`gpg_fingerprint`), then Sigstore
  and GitHub artifact attestations.

**Approval.** With `approval: manual`, new packages are stored with placement
state `held` until `garner approve`. The default is `auto` for all source
types.

## 8. Package name ownership

- Every package name has exactly one owner, shared by deb and rpm.
- An owner is a token's `owner` label (for example `team-platform`) or a
  source (`source:tool`). Ownership follows the label, not the token, so
  rotating tokens keeps ownership.
- First delivery of an unowned name assigns ownership.
- `owns:` globs in a source reserve names. On reload, a reservation that
  conflicts with an existing database owner wins for future deliveries.
  Existing packages stay, and the conflict is reported.
- Reservations of different sources must not overlap. If any name could
  match globs from two sources, `config validate` and reload reject the
  configuration, naming both sources, both globs and an example name that
  matches both. The check is exact: globs are regular patterns, so whether
  two can match the same name is decidable.
- Upload of a name owned by someone else: `409` with the owner's name; nothing
  is stored.
- Sync delivery of a name owned by someone else: the package is stored as
  `held` and a conflict is recorded.
- `garner owner set <name> <owner>` (admin scope) transfers ownership. Admin
  tokens do not bypass ownership implicitly.

## 9. Publishing, snapshots and garbage collection

### Publish

1. Lock: one publish at a time.
2. Select active placements and apply retention: keep the newest N versions
   per (name, distribution, arch), plus all `pinned` packages.
3. Render metadata into `snapshots/<id>/` (ULID): deb `Packages`, `Release`,
   by-hash; rpm `primary`, `filelists`, `other`, `repomd.xml`; the landing
   page, keys and client files.
4. Carry over index history: hardlink into the new snapshot's `by-hash/`
   (deb) and `repodata/` (rpm) the index files of every snapshot published
   within `storage.index_history` (default 1 h), and always those of the
   previous snapshot. Files are content-named, so names never clash. Copy if
   hardlinks are not possible.
5. Sign `InRelease`/`Release.gpg` and `repomd.xml.asc`.
6. fsync everything, reconcile the fixed links in `public/`, then switch
   `public/current` atomically: create a new symlink beside it and
   `rename(2)` it over the old one.
7. Mark the snapshot current in the database.

Step 4 closes the race between a client reading the signed top-level file
(`InRelease`, `repomd.xml`) and fetching the index files it lists. Without it,
a publish in between removes those files and the client fails with a hash
mismatch or a 404. The signed top-level files list only the new snapshot's
index files; the carried-over files are reachable by hash only.

Snapshots are immutable, so history is only ever added to a new snapshot.
This is why a rollback writes a new snapshot (see below) instead of
repointing `public/` to the old one.

rpm repodata generation uses `createrepo_c` in the MVP, behind an interface.
A native Go implementation can replace it later. garner carries over old
repodata itself instead of using `createrepo_c --retain-old-md`, because each
snapshot is rendered into a fresh directory.

rpm support needs `createrepo_c`, `rpmsign`, `gpg` and `gpgconf` on `PATH`
(the container image ships them). Without them, `serve` disables rpm and
says which tools are missing.

### Rollback and pinning

- `garner snapshot list` lists snapshots. `garner snapshot rollback <id>`
  publishes a new snapshot with the same package set as `<id>`: re-rendered,
  re-signed and with current index history (step 4), so clients mid-update
  are not broken. The old snapshot stays unchanged.
- Clients can pin to `/snapshots/<id>/...`.
- A rollback also sets placements to match the rolled-back set: its
  packages become active again and other active packages are removed, so
  the next publish keeps the rolled-back state.
- Snapshot retention: the newest `storage.snapshot_retention` (default 30,
  at least 2).

### Garbage collection

1. Retention removes packages from future snapshots only: their placements
   are marked `removed`.
2. A blob is deleted only when no retained snapshot references it.
3. A replaced snapshot is not deleted until `storage.gc_grace` has passed
   (default 10 min), so requests already resolved through it can finish.

## 10. Signing

- `garner init` generates an RSA 4096 key when none is configured. An
  existing key is imported by pointing `signing.keys[].key_file` at it.
- No passphrase by default (unattended signing). `passphrase_file` is
  optional.
- No expiry by default. When an expiry is set, a metric exposes the days
  remaining.
- All configured keys are published in `/key.asc` and `/key.gpg`. The first
  key signs.
- Rotation:
  1. Add the new key second.
  2. Wait for clients to fetch the keyring.
  3. Move the new key first.
  4. Remove the old key.

  apt `InRelease` can carry both signatures during the overlap. rpm packages
  carry one signature, so promoting a new key re-signs all rpm packages from
  their originals.
- rpm packages are re-signed with `rpmsign --addsign` (the container image
  ships `rpm-sign`). The original blob is kept, so packages can be re-signed
  after key rotation. Upstream checksums are verified before re-signing.
- deb packages are not signed individually. apt verifies the
  `InRelease` -> `Packages` -> sha256 chain.
- Signing goes through a `Signer` interface so HSM/KMS backends can be added.

## 11. Authentication and security

- Reading is open. The network is the boundary.
- Writes require a token. Scopes:
  - `upload`: add packages;
  - `publish`: trigger publish and sync, approve held packages, roll back;
  - `admin`: remove packages, manage owners and tokens, reload.
- Tokens are shown once at creation and stored as sha256 hashes.
  `garner init` prints an initial admin token.
- HTTP only; TLS terminates at a reverse proxy. Authenticated requests are
  accepted only:
  - from loopback, or
  - from an address in `server.trusted_proxies` with
    `X-Forwarded-Proto: https`.

  Otherwise they get `403`. A loopback request that carries
  `X-Forwarded-Proto` comes from a proxy on the same host, so that header
  must say `https` too. `allow_insecure_auth: true` disables the check for
  isolated labs.
- Only `X-Forwarded-*` headers from trusted proxies are honoured.

## 12. Storage and database

### Storage

- A local filesystem path. Atomic swaps use `rename(2)`, and writes are
  fsynced before the rename.
- All storage access goes through an interface (blobs, pool, snapshots,
  pointers). S3-compatible storage is planned after the MVP, with
  `garner storage migrate`.

### Database

- SQLite (`modernc.org/sqlite`, no cgo), with the database on local disk,
  never on NFS.
- PostgreSQL comes with multiple nodes (section 18).

## 13. Observability

- Logs: `log/slog` to stdout, JSON or text.
- `/healthz`: the process is alive. `/readyz`: the database and storage are
  reachable and a current snapshot exists.
- Prometheus `/metrics`, optionally on a separate listener. Metrics:
  - `garner_sync_last_success_timestamp_seconds{source}`
  - `garner_sync_errors_total{source}`
  - `garner_sync_packages_added_total{source}`
  - `garner_forge_ratelimit_remaining{forge}`
  - `garner_publish_duration_seconds`
  - `garner_publish_last_success_timestamp_seconds`
  - `garner_packages{format,distribution,arch}`
  - `garner_uploads_total{result}`
  - `garner_upload_bytes_total`
  - `garner_conflicts_open`
  - `garner_storage_bytes`
  - `garner_snapshots`
  - `garner_signing_key_expiry_days`
  - `garner_http_requests_total{route,code}`
- Alerting is left to Prometheus. There are no built-in notifications.

## 14. Interfaces

### CLI

Commands marked *local* operate on the database directly and must run where
the configuration and database are reachable. The others call the API with
`--url` (or `GARNER_URL`) and `GARNER_TOKEN=<name>:<secret>`.

| Command | Mode | Scope |
|---|---|---|
| `serve` | local | |
| `init` | local | |
| `config validate FILE` | local | |
| `reload` | API | admin |
| `token create --name --owner --scope` / `list` / `revoke` | local or API | admin |
| `upload FILES` | API | upload |
| `remove NAME [--version] [--arch]` | API | admin |
| `owner list` / `owner set NAME OWNER` | API | admin |
| `sync SOURCE` | API | publish |
| `publish` | API | publish |
| `packages list` | API | none |
| `approve PACKAGE` / `conflicts list` | API | publish |
| `snapshot list` / `snapshot rollback ID` | API | publish |
| `gc` | API | admin |
| `status` | API | none |

### HTTP API (v1)

| Method and path | Scope |
|---|---|
| `PUT /api/v1/upload` | upload |
| `GET /api/v1/status` | none |
| `GET /api/v1/packages` | none |
| `DELETE /api/v1/packages/{name}` | admin |
| `GET /api/v1/owners` | none |
| `PUT /api/v1/owners/{name}` | admin |
| `POST /api/v1/sources/{name}/sync` | publish |
| `GET /api/v1/conflicts` | publish |
| `POST /api/v1/packages/{id}/approve` | publish |
| `GET /api/v1/snapshots` | none |
| `POST /api/v1/snapshots/{id}/rollback` | publish |
| `POST /api/v1/reload` | admin |
| `POST /api/v1/gc` | admin |
| `POST /api/v1/publish` | publish |
| `POST/GET/DELETE /api/v1/tokens` | admin |

### Landing page

A static page, rendered into each snapshot at publish (section 4). Its
static part describes that snapshot only:

- install commands for the key and sources (deb822 and `.repo`);
- package counts per format, distribution and architecture;
- time of the publish;
- configuration hash and commit the snapshot was published with.

A live status section shows state that changes without a publish: per-source
last sync and error state, and open conflicts. A small inline script fetches
it from `GET /api/v1/status`. When garner is not reachable, for example while
the proxy serves `public/` and garner is stopped, the section says that the
status is unavailable; the rest of the page still works.

There are no download statistics, no login and no write actions.

## 15. Scope

### MVP

1. `serve`, `init`, `config validate`, reload, SQLite, local storage, signing,
   atomic publish with snapshots and rollback, landing page and client files.
2. Upload API and CLI, tokens with scopes and owners, name ownership.
3. Forge sync for GitHub (single repo and org) with asset rules, verification,
   retention and approval.
4. Forgejo/Gitea and GitLab clients, GitHub Enterprise Server.
5. Metrics, health endpoints, container image, static binary, systemd unit
   example.

### Later

- PostgreSQL, multiple nodes and `db migrate` (section 18).
- S3-compatible storage and `storage migrate`.
- Webhooks.
- Upstream GPG fingerprint pinning, then Sigstore and attestations.
- Stage suites with a `promote` command.
- OIDC federation for CI and SSO for people.
- Named and aggregate repositories.
- Built-in git config fetch.
- Helm chart.
- Further package formats, for example npm, PyPI, Ansible collections and OCI
  (section 17).
- Exclusions in `owns:` (`["tool-*", "!tool-cli"]`), so one source can
  reserve a pattern while another owns an exception. The overlap check
  (section 8) takes exclusions into account.
- HSM/KMS signing.
- A native rpm repodata writer and rpm signing (removing the `createrepo_c` and
  `rpmsign` dependencies).
- Separate serve/worker roles.
- Audit log (`events` table): who uploaded, removed or transferred
  ownership of what.
- Test clients on the previous release of each distribution as well
  (Ubuntu 24.04 LTS, EL 9), since enterprise fleets lag the latest release.
- deb components beyond `main`, mapped per source or asset rule. Adding
  components does not affect clients that list only `main`.

## 16. Open questions

- **License:** deliberately undecided until release is decided. Chosen
  dependencies (`modernc.org/sqlite`, `ProtonMail/go-crypto`, `goose`) are
  BSD/MIT. `rpmsign` and `createrepo_c` run as external processes.

## 17. Future package formats

More package formats will be added later. The set is open: the formats below
are examples, not a list to design against. None is in the MVP, but the MVP
should not make any new format expensive to add.

### Examples, and how they differ from deb and rpm

The examples show how wide the range is. The MVP's abstractions must cover all
of them, not just the formats that resemble deb and rpm.

| Format | Client protocol | Fits static snapshots | Repository signing | Version order |
|---|---|---|---|---|
| npm | Registry JSON (packuments); `npm publish` is a JSON PUT | Mostly: packuments and tarballs are files, but scoped names (`@scope%2fname`) need care behind nginx | None; npm uses provenance and registry signatures instead | semver; dist-tags |
| PyPI | Simple API (PEP 503/691); `twine upload` uses the legacy multipart API | Yes | None; PEP 740 attestations | PEP 440 |
| Ansible | Galaxy API v3 | Possibly, if the API responses are pre-rendered; unverified | None | semver |
| OCI | Distribution spec, `/v2/` at the host root | No: tag resolution, `HEAD`, content negotiation and push need a live API | Separate artifacts (cosign, notation) | Tags are unordered |

### Constraints on the MVP

- **Format interface.** deb and rpm are implemented behind one interface, and
  the core never branches on a format name. A format provides detection and
  parsing, name normalization, version comparison (or none, for formats whose
  versions are unordered), metadata rendering into a snapshot, and optionally
  live HTTP endpoints. Adding a format adds an implementation and registers
  it.
- **Data model.** The generic identity of a package is (format, name,
  version). `epoch`, `release`, `arch` and `distribution` are format-specific;
  other formats store their own fields in `metadata`. No table or column is
  named after a specific format.
- **Ownership namespaces.** Names are owned per namespace, because the same
  name in two ecosystems usually means unrelated projects. Each format
  declares its namespace; formats that share a name space share one, as deb
  and rpm share `os`. The MVP adds the `namespace` column now, always `os`,
  so no data migration is needed later.
- **URL space.** Each format is served under its own prefix, named after the
  format (`/deb/`, `/rpm/`, and so on). Some clients require fixed paths at
  the host root (OCI requires `/v2/`); a format declares such paths, and the
  server rejects two formats claiming the same one. garner's own routes stay
  under `/api/`, `/snapshots/` and the few files listed in section 4.
- **Configuration.** Format settings live in a section named after the
  format, as `deb:` and `rpm:` do. Validation is delegated to the format.
- **Serving model.** A format either renders static files into snapshots (deb,
  rpm, and probably PyPI) or needs live endpoints served by garner (OCI).
  Both are supported by the interface. A live format cannot be served by
  the proxy from `public/` alone, which the deployment docs must state.
- **Trust.** "Clients trust exactly one key" holds only for formats with
  repository signing, such as deb and rpm. Others rely on TLS plus the
  upstream verification garner does at ingest.
- **Retention.** Each format sets its retention default. Language ecosystems
  pin exact versions in lockfiles, so they will usually keep every version.
- **Upload.** `PUT /api/v1/upload` asks each registered format to detect the
  file. Native client protocols (`npm publish`, `twine upload`, `docker push`)
  are per-format endpoints.
- **Sync.** Forge releases rarely carry most of these formats; forge package
  registries (GitHub Packages, GitLab Package Registry) are the likelier
  source. Asset rules will need a format-aware target in place of
  `suite`/`release`.

Of the examples, OCI fits the current design least: it needs live endpoints,
and its blobs and manifests behave differently from the package pool.

## 18. Multiple nodes (later)

Not in the MVP. When the reverse proxy serves `public/` from disk, reads
stay available while garner is down (section 3), so multiple nodes mainly
protect uploads and sync. Deployments using the built-in server get no read
availability from that setup. This
section keeps the earlier design so the MVP does not close it off.

### Design

- Every node runs `garner serve` and serves the repository, the landing page
  and the API, and accepts uploads.
- One node is the leader and runs sync, publish, retention and garbage
  collection. Leadership is a PostgreSQL session-level advisory lock; if the
  leader dies, its connection drops and another node takes the lock.
- A node that ingests an upload notifies the leader with `LISTEN/NOTIFY`.
- Storage is shared (NFSv4 or CephFS): atomic swaps use `rename(2)`, writes are
  fsynced before rename, locks live in the database and never in `flock` on
  shared storage, and GC respects `gc_grace`, which must then cover the NFS
  clients' `actimeo`.
- `database.url_file` holds the PostgreSQL URL.
- Possible later still: separate roles (`--role=serve|worker`).
- rpm re-signing happens at ingest (section 7), so every node that accepts
  uploads needs `rpmsign`, `gpg` and the private key. Revisit whether to move
  it to the leader at publish time.

### Problems to solve first

- **Stale leader.** A leader that loses its database connection during a
  publish can keep writing while a new leader starts. The leader takes an
  increasing fencing token from the database, and the `public/current` swap
  is refused unless that token is still current.
- **Connection poolers.** Session advisory locks and `LISTEN/NOTIFY` do not
  work through PgBouncer in transaction mode. Require a direct connection or
  a session-mode pooler, and make `/readyz` detect a lock that does not hold.

### Migration from SQLite to PostgreSQL

1. Stop garner. `db migrate` refuses to run while the SQLite lock is held.
2. Run
   `garner db migrate --from sqlite:///var/lib/garner/garner.db --to postgres://...`.
   It applies the schema migrations to PostgreSQL, copies every table in one
   transaction, then verifies row counts and a per-table checksum.
3. Point `database.url` at PostgreSQL and start garner. Add nodes after
   moving `storage.path` to shared storage if needed.

The SQLite file is left untouched; rollback is pointing the configuration back
at it. Migration is one-way. Files in storage are not touched. While garner
is stopped, a proxy serving `public/` from disk keeps serving it.

### Constraints on the MVP

- **Portable schema.** No SQLite-only types or functions (section 6). CI
  applies every migration to PostgreSQL.
- **Narrow interfaces** for the database, locks and publish requests. The MVP
  implements them in-process; PostgreSQL implementations slot in behind the
  same calls. Code never relies on a Go mutex or channel where a database lock
  or notification will later be needed.
- **One swap point.** Publish ends in a single `rename(2)` of
  `public/current` (section 9), which is the one place a fencing check will
  guard.
