# garner design

Status: MVP implemented (see `plan.md`). Decisions recorded 2026-10-13,
updated 2026-11-14 with choices made during implementation.

## 1. Purpose

garner is an internal repository for packages and container images. Its
core is format-agnostic (section 17); the formats today are `.deb` and
`.rpm` packages, served as signed apt and dnf repositories, and OCI images,
served as a registry (section 19). It accepts packages from two places:

- uploads, typically from CI pipelines;
- release assets synced from GitHub, GitHub Enterprise Server, GitLab and
  Forgejo/Gitea, from single repositories or from whole organizations/groups.

Images arrive the same two ways: pushed with `docker push`, and synced from
registries such as ghcr.io and Docker Hub.

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
- Formats other than deb and rpm in the MVP. OCI images came after it
  (section 19), and more are expected; section 17 lists what the core must
  not rule out.
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
  the UI and the API, accepts uploads, and runs sync, publish,
  retention and garbage collection.
- At startup it takes an `flock` on `<db>.lock` and refuses to start if
  another process holds it.
- Ingest records a package in the database and requests a publish. Publish
  requests are debounced (default 10 s) so a burst of uploads yields one
  publish. A failed publish is retried with backoff (30 s, doubling to
  10 min), so a transient error does not leave changes unpublished.
- Under systemd, garner runs as `Type=notify`: it reports readiness after
  its first publish and keeps its listening sockets in systemd's file
  descriptor store. On a restart the socket stays open, so requests wait
  for the new process instead of being refused. The address still comes
  from `server.listen`; when it changes, garner replaces the stored socket.
- `public/` is plain files. When the reverse proxy serves it directly from
  disk (section 4), apt and dnf clients keep working while garner is stopped
  or restarting; only uploads and sync pause. With the built-in server, reads
  stop too.

Multiple nodes and separate roles are later extensions (section 18).

## 4. Repository layout

### Public URLs

```
/                         UI (static per snapshot, live status)
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
/v2/...                   OCI registry, with the oci section (section 19)
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

Client example (the UI renders this):

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
- `i686` is multilib only: with `i686` and `x86_64` configured, the x86_64
  repository also lists the i686 packages, as Fedora and EL ship them, and
  there is no `i686` repository. dnf on an x86_64 host reads only the
  x86_64 one (`$basearch`), and no supported host runs i686. Configuring
  `i686` requires `x86_64`.
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
deb, rpm, the UI, the keys and the client files together, and a
client never sees two snapshots at once. A publish creates fixed links that
are missing (a new suite, release or architecture) and removes those no
longer configured; it never retargets them.

The UI, keys and client files belong to the snapshot so that they
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

garner is tested against every supported release of each client
distribution: Debian, Ubuntu LTS, RHEL and its rebuilds (AlmaLinux, Rocky),
and Fedora. As of 2026-11 that is Debian 12 and 13, Ubuntu 22.04, 24.04 and
26.04 LTS, EL 9 (AlmaLinux) and EL 10 (AlmaLinux and Rocky Linux), and
Fedora 44.

They check signatures with different tools: apt with Sequoia's `sqv` on
Debian 13 and with GnuPG's `gpgv` on Debian 12 and Ubuntu; rpm with
`rpm-sequoia` on EL 10 and Fedora, and without it on EL 9. garner signs
with RSA 4096 and SHA-256 or stronger, which all of them accept; the
Sequoia-based ones reject SHA-1.

## 5. Configuration

A single YAML file holds human decisions. The database holds state.

- Default path: `/etc/garner/garner.yaml`; override with `--config`.
- Secrets are never inline. Use `*_file` (Kubernetes secrets, systemd
  `LoadCredential=`) or `*_env`.
- `garner config validate <file>` validates offline and can run in CI.
- Reload with `SIGHUP` or `garner reload`. An invalid file is rejected,
  logged, and the last good configuration stays active.
- The configuration hash (and the git commit, if the file is inside a git
  working tree) is shown in the UI and in `/api/v1/status`.
- The CLI never modifies anything the file defines.

[configuration.md](configuration.md) lists every key with its default and
validation rules.

### Example

```yaml
server:
  listen: ":8080"
  public_url: https://repo.example.com
  client_name: garner
  trusted_proxies: ["10.0.0.0/8"]
  allow_insecure_auth: false

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
  max_package_size: 2GiB      # largest upload or synced asset

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
  min_release_age: 0s         # e.g. 336h: sync releases once two weeks old

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
    checksums: ["tool_*_SHA256SUMS"]   # replaces the built-in names

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

A format is enabled by its section being present (`deb:`, `rpm:`); an empty
section uses the defaults, and at least one format is required. Adding or
removing a section takes a restart, because formats are built once at
start. A removed section keeps the format's packages stored but unserved.

## 6. Data model

The schema is portable across SQLite and PostgreSQL: no `JSONB`, no arrays, no
dialect-specific functions in the core schema. Structured fields are `TEXT`
holding JSON, read only by the application. Migrations use `pressly/goose`.
The MVP supports SQLite only. To keep the schema portable, CI also applies all
migrations to PostgreSQL; the test suite itself runs on SQLite.

| Table | Key columns | Notes |
|---|---|---|
| `repositories` | `id`, `name`, `signed_with` | One row (`default`) in the MVP. `signed_with` is the fingerprint of the key packages were last re-signed with. Every other table references it, so named repositories can be added without a data migration. |
| `blobs` | `sha256` PK, `size`, `created_at` | Originals and re-signed variants. |
| `packages` | `id`, `repository_id`, `format`, `name`, `epoch`, `version`, `release`, `arch`, `blob_sha256`, `signed_blob_sha256`, `metadata` (TEXT/JSON), `origin` (`upload`/`source`), `origin_ref`, `verify_status`, `pinned`, `created_at` | Unique on (`repository_id`, `format`, `name`, `epoch`, `version`, `release`, `arch`, `distribution`) through `placements`. |
| `placements` | `package_id`, `distribution`, `state` (`active`/`held`/`removed`), `owner_conflict`, `created_at` | Where a package is published. `owner_conflict` marks a hold whose deliverer did not own the name (section 8). |
| `name_owners` | `repository_id`, `namespace`, `name` PK, `owner`, `source` (`first`/`config`/`manual`), `assigned_at` | One owner per name within a namespace. deb and rpm share the namespace `os`, the only one in the MVP (section 17). |
| `conflicts` | `id`, `kind`, `package_ref`, `detail`, `created_at`, `resolved_at` | Ownership, content, asset-mapping, checksum and download conflicts. |
| `source_state` | `source` PK, `last_run`, `last_success`, `last_error`, `etags` (TEXT/JSON) | |
| `source_repos` | `source`, `repo`, `etag`, `last_seen` | Cached org/group listing. |
| `seen_releases` | `source`, `repo`, `tag`, `release_id`, `processed_at` | Prevents reprocessing. |
| `snapshots` | `id`, `created_at`, `config_hash`, `is_current`, `replaced_at` | `replaced_at`: when the snapshot stopped being current, for `gc_grace`. |
| `snapshot_packages` | `snapshot_id`, `package_id`, `distribution`, `served_sha256` | Drives GC reachability. `served_sha256` is the blob the snapshot serves the package from, kept after a re-sign replaces it. |
| `snapshot_formats` | `snapshot_id`, `format` | The formats a snapshot rendered. A rollback changes placements only for these. |
| `tokens` | `id`, `name`, `owner`, `scopes`, `hash`, `created_at`, `last_used_at`, `revoked_at` | sha256 of a 256-bit random secret. |
| `events` | `id`, `at`, `actor`, `action`, `detail` | Audit log. Later, not in the MVP (section 15). |
| `package_blobs` | `package_id`, `sha256` | Further blobs a package owns, such as an image's layers (section 19). |
| `tags` | `repository_id`, `format`, `name`, `tag` PK, `package_id`, `updated_at`, `updated_by` | Mutable labels on packages: image tags. |
| `snapshot_tags` | `snapshot_id`, `format`, `name`, `tag`, `package_id` | The tags a snapshot served, for rollback. |
| `staged_blobs` | `sha256` PK, `expires_at` | Blobs a push or sync stored before a package references them; GC keeps them until `expires_at`. |
| `uploads` | `id`, `format`, `name`, `size`, `created_at`, `updated_at` | Blob upload sessions of pushes in progress. |
| `source_tags` | `source`, `repo`, `tag` PK, `digest`, `applied_at` | The upstream digest a registry source's tag was last applied from. |
| `source_tag_candidates` | `source`, `repo`, `tag`, `digest` PK, `first_seen` | Digests seen under a tag, for `min_release_age`. |

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
- `413` when the file exceeds `storage.max_package_size` (default 2 GiB).
- Uploads are published immediately (`approval` does not apply).
- `garner upload [--suite S] [--release R] FILES...` wraps the API.

```
curl -fsS -u ci:$GARNER_TOKEN -T tool_1.4.0_amd64.deb \
  "https://repo.example.com/api/v1/upload?suite=stable"
```

### Forge sync

**Polling.** Each source has a `poll_interval`, with ±10 % jitter. Requests
use `If-None-Match` with stored ETags. `garner sync <source>` triggers an
immediate run. A new source, at startup or after a reload, first polls
within a random 0–30 s. A reload restarts only sources whose settings
(including inherited defaults and the forge's settings) changed; the
others keep their schedule. It does not wait for running syncs: a changed
or removed source's sync is cancelled, and its unfinished release is
retried by the next run. Webhooks come later and will only trigger a poll; the payload is
not trusted.

**Forge clients.**

| Type | Releases API | Asset download |
|---|---|---|
| `github` | `/repos/{o}/{r}/releases` | the API asset `url` with `Accept: application/octet-stream` (works for private repositories); `digest` field when present |
| `forgejo`/`gitea` | `/api/v1/repos/{o}/{r}/releases` | `browser_download_url` |
| `gitlab` | `/api/v4/projects/{id}/releases` | `assets.links[]` (often Generic Package Registry or external URLs) |

A release's publish time is `published_at` on GitHub and Forgejo. On
GitLab it is the later of `created_at` and `released_at`: the caller may
set `released_at` to a past date (a historical release), and only
`created_at` is set by the server. The listing keeps GitLab's
`released_at` order, which places an upcoming release where it comes out;
a backdated release sorts far back and may not be listed at all, which
publishes nothing early. GitHub and Forgejo also date each asset
(`created_at`, and on GitHub the later `updated_at`, which a rename moves);
GitLab links carry no time. GitLab has no release IDs, so garner uses the
tag plus `created_at`: a release deleted and re-created under the same tag
is a new release. Records made under the bare tag by an older garner
still count as processed (the release's former ID). A single release is
also fetched by ID
(`/releases/{id}`, on GitLab `/releases/{tag}`), for pending releases a
listing does not reach.

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
- With `min_release_age` (default `0s`, off), a release is processed only
  once it is that old. Its age runs from its publish time or, where the
  forge dates assets, from the upload of its newest candidate asset if
  that is later. A younger release is deferred: not processed, and
  recorded as pending, so later polls list it again even after a newer
  release was processed. The listing's ETag is not stored while a release
  is deferred, or "not modified" would hide it. A release without a
  publish time is deferred until an override. This gives a compromised
  release time to be noticed and pulled before garner publishes it. A
  release without package assets is not deferred: nothing in it can be
  published, and assets attached later change the listing and wait then.
  A pending release whose package assets were removed is dropped.
  Uploads are not affected.
- First run of a source (or a newly discovered repo): only the latest
  published release with candidate assets, or the latest if none has
  any; with `min_release_age`, the latest that is old
  enough, after which each newer release follows as it ages. A repository
  that publishes several products as separate releases
  (`cmd/builder/v1.2.0` next to `v1.2.0`) may list one whose assets match
  nothing last; starting there would wait for its assets forever. A new gated
  source can therefore publish several recent versions over its first
  days, where an ungated one publishes only the newest; waiting for the
  newest to age instead would publish nothing for a project that releases
  more often than `min_release_age`. Retention trims the extra versions.
- Later runs: every release published after the last processed one, or in
  the same second, ordered by the forge's publish timestamp
  (`published_at` or equivalent). Tags are
  not compared as versions, because many are not (`nightly`,
  `2024-05-release`). `seen_releases` keeps a release from being processed
  twice, also when its timestamp is edited.
- The newest release is not marked processed while it has no candidate
  assets, because many CI pipelines publish a release and attach assets
  afterwards. A newer release ends the wait. Assets added to a release that
  already had candidates are not picked up. With `min_release_age`, assets
  attached later wait for their own age where the forge dates them. Where
  it does not (GitLab), an old-enough release is processed as it is, since
  undated assets attached to it later would skip the age; one let through
  by an override still waits for its assets.
- `garner sync <source> --ignore-age <tag> [--repo <repo>]` lets one
  release skip `min_release_age`, for an urgent fix. It needs the
  `publish` and `admin` scopes, is logged, and is kept in
  `release_age_overrides` with the token's name. It covers a release only
  if the release and its dated candidate assets predate the override, so
  a release re-created under the same tag, or an asset uploaded after the
  override, waits again; run the override once the assets are attached.
  On an org or group source, the repository must be one the last listing
  found; its case is taken from the listing. A tag already processed is
  refused with 409, since assets attached to it since are never picked up,
  unless a release under that tag is deferred: one deleted and re-created
  upstream is a new release. An override for a release the source never
  processes (older than where it starts), or one the release or an asset
  outgrew (changed after the override, or a forge clock ahead of garner's),
  is logged once per process. The request body is strict: an unknown
  key or trailing data is a 400. If the sync cannot start, the stored
  override still answers 202 and applies on the next poll. An override on
  a source without `min_release_age` is refused with 409.
- A pending release, deferred or failed, that the listing does not hold
  is fetched by ID: listings stop after a few pages and are sorted by
  creation, not publish time. If it is gone, or became a draft or
  pre-release, as when a compromised release is pulled during the wait,
  it is dropped from `pending_releases` with a warning, so later listings
  stop reaching back to it. If the forge now names it differently, it stays
  pending under its new ID. A failed lookup holds up only that release: it
  stays pending, the run reports the error, and the repository's other
  releases go ahead. While a repository has pending releases its listing is fetched
  without its ETag: the listing may again match one stored before the
  release appeared. While a listing fails, the repository's deferred
  releases stay in status.
- If upstream deletes a release, the packages stay; retention handles them.
- A failed download leaves the release pending: every later run retries
  it, fetching the release listing far enough back to reach it, and newer
  releases are processed meanwhile. The exception is a failure that
  cannot succeed: HTTP 404 or 410 from the forge's own origin, or an asset
  larger than `storage.max_package_size`. A 404 from a link outside the
  origin was fetched without the token and may only mean the repository is
  private, so it is retried. For a skipped asset a `download` conflict is
  recorded; for a checksum file, the whole release is skipped, since its
  assets cannot be verified.
- An asset rejected for its content (invalid package, content or ownership
  conflict) does not hold the release back. Any other ingest failure (disk,
  database, signing) leaves the release pending the same way.

**Asset selection.**

- Candidates: assets with the extension of an enabled format (`*.deb`,
  `*.rpm`), excluding `*.src.rpm`,
  `*-dbgsym*`, `*-debuginfo*`, plus the source's `exclude` list. With asset
  rules, an asset whose name matches no rule is not a candidate either.
  These checks use the name only, so other assets are never downloaded.
  The architecture is read from the package, after download; `exclude`
  keeps unwanted architectures from being downloaded at all.
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
  `*.sha256`, or the source's `checksums:` globs, which replace those
  names), the asset must match it. Lines are `HASH  NAME` or `HASH *NAME`;
  a file holding only a hash applies to the asset named like the file
  without its last extension (`tool.deb.sha256sum` covers `tool.deb`). A
  mismatch rejects the asset.
- Checksum files are read only for candidates without a forge digest,
  since the digest wins; when every candidate has one, none is read. A
  checksum file named after another asset of the release (`tool.deb.sha256`
  next to `tool.deb`) covers only that asset and is read only when that
  asset needs it; any other checksum file is always read. Releases can
  carry hundreds of such files, one for every archive and SBOM, and
  reading them all exhausts an unauthenticated API rate limit.
- `verify: opportunistic` accepts assets without a checksum and marks them
  `unverified`. `verify: required` rejects them.
- Later: checking that a release was signed by a signer the source
  trusts (section 20).

**Approval.** With `approval: manual`, new packages are stored with placement
state `held` until `garner approve`. The default is `auto` for all source
types. Approval applies the content check of section 7: it fails with `409`
when another file of the same version is already in the distribution.

## 8. Package name ownership

- Every package name has exactly one owner, shared by deb and rpm.
- An owner is a token's `owner` label (for example `team-platform`) or a
  source (`source:tool`). Ownership follows the label, not the token, so
  rotating tokens keeps ownership.
- First delivery of an unowned name assigns ownership.
- A package also needs the names it replaces on clients: deb `Replaces` and
  rpm `Obsoletes`. They are checked like the package's own name, and a free
  one is assigned to the uploader. A name of another owner gives the same
  `409` or held state as an upload of that name. This keeps an owner of
  `a` from shipping `Obsoletes: b`, which dnf would apply on a plain
  upgrade. `Provides` is not checked: virtual names such as `webserver`
  have many providers, and clients prefer a real package of that name.
  Packages stored before this rule are not re-checked.
- `owns:` globs in a source reserve names. In a glob, `*` and `?` match any
  character, `/` included, so `team/*` reserves every image below `team/`;
  validation's overlap check and ingest use the same matcher. A source
  reserves names only in the namespaces of what it delivers: forge sources
  package names (`os`), registry sources image names (`oci`), so the same
  glob in one of each is no conflict (section 19). On reload, a reservation that
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
  `held` and a conflict is recorded. It claims none of its names, and until
  it is approved it does not take part in the content check (section 7), so
  the owner can still deliver that version. Approving it publishes it under
  another owner's name, so it needs the `admin` scope as well as `publish`.
- `garner owner set <name> <owner>` (admin scope) transfers ownership. Admin
  tokens do not bypass ownership implicitly.

## 9. Publishing, snapshots and garbage collection

### Publish

1. Lock: one publish at a time.
2. Select active placements and apply retention: keep the newest N versions
   per (name, distribution, arch), plus all `pinned` packages.
3. Render metadata into `snapshots/<id>/` (ULID): deb `Packages`, `Release`,
   by-hash; rpm `primary`, `filelists`, `other`, `repomd.xml`; the UI,
   keys and client files.
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

Each snapshot records a digest of its inputs: garner's version, the
configuration hash and commit, the enabled formats, the signing keys and the
package set after retention. A start publishes only when that digest differs
from the current snapshot's, so restarts do not push real history out of
`snapshot_retention`, while a changed binary, key or installed rpm tool is
still published.

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
(the container image ships them). With `rpm:` configured and a tool
missing, `serve` refuses to start and says which tools are missing.

### Rollback and pinning

- `garner snapshot list` lists snapshots. `garner snapshot rollback <id>`
  publishes a new snapshot with the same package set as `<id>`: re-rendered,
  re-signed and with current index history (step 4), so clients mid-update
  are not broken. The old snapshot stays unchanged.
- Clients can pin to `/snapshots/<id>/...`.
- A rollback also sets placements to match the rolled-back set: its
  packages become active again and other active packages are removed, so
  the next publish keeps the rolled-back state. Only formats the snapshot
  rendered take part: a format that was disabled then is missing from the
  set without its packages being gone, so they keep their state.
- Snapshot retention: the newest `storage.snapshot_retention` (default 30,
  at least 2).

### Garbage collection

1. Retention removes packages from future snapshots only: their placements
   are marked `removed`.
2. A blob is deleted only when no retained snapshot references it.
3. A replaced snapshot is not deleted until `storage.gc_grace` has passed
   (default 10 min) since it stopped being current, so requests already
   resolved through it can finish. A snapshot that never became current,
   left by a publish that crashed before the swap, gets the grace from its
   creation. A crash before the snapshot is recorded leaves a directory
   without a row, removed once `gc_grace` has passed since it was
   written. A publish that fails without crashing, cancelled ones
   included, removes its snapshot.

### Verification

`garner verify` (`POST /api/v1/verify`) checks the published tree against
the database without changing anything, under the publish lock so no
publish or GC runs meanwhile. It is for use after upgrading or patching
garner, after a GC or a restore, when a client reports a hash mismatch,
and in CI after a configuration push.

- Core: `public/current` points to the snapshot the database marks
  current; snapshot rows and directories match; the fixed links resolve;
  `key.asc` and `key.gpg` hold the configured keys; every served package
  has its blob and pool file with the recorded size.
- Each format reads its metadata as its clients do, from the signature
  down: deb checks `InRelease` and `Release.gpg`, every file `Release`
  lists and its by-hash copy, the compressed `Packages` and each stanza
  against the pool and the package set; rpm checks `repomd.xml.asc`,
  every repodata file before and after decompression, and `primary`
  against the pool and the package set. Both check that the previous
  snapshot's index files are still served (step 4 of Publish). The formats
  parse the files themselves rather than reusing the renderers.
- An error is a state a client can fail on. A warning is one the next
  publish or GC repairs: a pending publish, a link no longer configured,
  and pool files, blobs, snapshot directories and temp files nothing
  references (the last three once older than `gc_grace`).
- Contents are compared with recorded sizes and hashes; blobs are not
  re-hashed.
- `config_hash` (a sha256 prefix of the configuration file) and `ready=1`
  make the endpoint answer `409` while another configuration is loaded or
  a publish is pending; `garner verify --wait` and `--config-hash` retry
  until then. The CLI exits 1 on errors, with `--strict` on warnings too.

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
  their originals. garner compares the signing key with the one recorded
  after the last re-sign at every start and reload, so a promotion takes
  effect through a restart too, and an interrupted re-sign is redone.
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
  - `publish`: trigger publish and sync, approve held packages (a package
    held for an ownership conflict also needs `admin`), roll back;
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
  isolated labs. The CLI likewise sends a token only over https or to
  loopback, unless `GARNER_INSECURE_AUTH=1` is set, so a mistyped
  `http://` URL does not put the token on the network before the server
  refuses it.
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
  - `garner_sync_releases_deferred{source}`
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
| `sync SOURCE --ignore-age TAG [--repo REPO]` | API | publish and admin |
| `publish` | API | publish |
| `packages list` | API | none |
| `approve PACKAGE` / `conflicts list` | API | publish (approving an ownership hold: publish and admin) |
| `snapshot list` / `snapshot rollback ID` | API | publish |
| `gc` | API | admin |
| `verify` | API | publish |
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
| `POST /api/v1/sources/{name}/sync` | publish; publish and admin with `ignore_age` |
| `GET /api/v1/conflicts` | publish |
| `POST /api/v1/packages/{id}/approve` | publish; publish and admin for an ownership hold |
| `GET /api/v1/snapshots` | none |
| `POST /api/v1/snapshots/{id}/rollback` | publish |
| `POST /api/v1/reload` | admin |
| `POST /api/v1/gc` | admin |
| `POST /api/v1/publish` | publish |
| `POST /api/v1/verify` | publish |
| `POST/GET/DELETE /api/v1/tokens` | admin |

### UI

A static page, rendered into each snapshot at publish (section 4). It is
organized by ecosystem: the enabled formats of one namespace (deb and rpm
are "Linux packages", oci "Container images"). Each ecosystem is a tab;
with one ecosystem there is no tab bar, and without script every
ecosystem is shown one below the other. The core takes labels and
content from format hooks (`Presenter`, `Lister`) and never branches on
a format name.

A tab starts with a summary:

- how to use it: install commands for the key and sources (deb822 and
  `.repo`), or how to pull and push images;
- what it holds: package counts per format, distribution and
  architecture, or images, tags, platforms and images per top-level
  namespace (the 12 largest);
- the signing key, in the tabs of formats clients check with it;
- the health of the sync sources that deliver it.

Below the summary the tab lists everything: each package name with its
versions per distribution and architecture, or each image with its
newest five tags (the rest at `/v2/<name>/tags/list`), platforms and a
button that copies its pull command. The page's script shows the list 25 names at a time, with
a filter box that narrows it before paging, so a tab stays short however
many packages there are. Without script the list shows in full. The
whole list is in the page, so `index.html` grows with the number of
names: about 150 KB for 500 images.

The header shows the publish time and one health line for all sources
and open conflicts; the snapshot ID, publish time, configuration hash
and commit are one line in the page footer.

The live parts, source health and conflicts, are state that changes
without a publish. A small inline script fetches them from
`GET /api/v1/status`, whose `kind` (`forge` or `registry`) places each
source under its ecosystem. When garner is not reachable, for example
while the proxy serves `public/` and garner is stopped, they say that the
status is unavailable; the rest of the page still works.

There are no download statistics, no login and no write actions.

## 15. Scope

### MVP

1. `serve`, `init`, `config validate`, reload, SQLite, local storage, signing,
   atomic publish with snapshots and rollback, UI and client files.
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
- Upstream signatures: OpenPGP, Sigstore and GitHub attestations (section 20).
- Stage suites with a `promote` command.
- OIDC federation for CI and SSO for people.
- Named and aggregate repositories.
- Built-in git config fetch.
- Helm chart.
- Further package formats, for example npm, PyPI and Ansible collections
  (section 17). OCI images are designed in section 19.
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
| OCI | Distribution spec, `/v2/` at the host root | Pulls yes, with `Content-Type` set per manifest type; push needs a live API (section 19) | Separate artifacts (cosign, notation) | Tags are unordered |

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
  format, as `deb:` and `rpm:` do, and the section's presence enables the
  format. Validation is delegated to the format.
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

Of the examples, OCI fits the current design least: push needs live
endpoints, and an image is many shared blobs under mutable tags. Section 19
designs it.

## 18. Multiple nodes (later)

Not in the MVP. When the reverse proxy serves `public/` from disk, reads
stay available while garner is down (section 3), so multiple nodes mainly
protect uploads and sync. Deployments using the built-in server get no read
availability from that setup. This
section keeps the earlier design so the MVP does not close it off.

### Design

- Every node runs `garner serve` and serves the repository, the UI
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

## 19. OCI images (after 0.1.0)

Built after 0.1.0 (`internal/oci`); `docs/plan.md` records what is
verified. garner stores container images (OCI and Docker schema 2
manifests and indexes), accepts them by `docker push` and `podman push`,
syncs selected images from other registries, and serves pulls from the
snapshot as it serves deb and rpm. It is not a pull-through cache: only
images pushed to garner or named by a source are stored (section 1).

### Model

- A package is one manifest or image index: format `oci`, name the
  repository path (`team/tool`), version the manifest digest
  (`sha256:<hex>`), arch empty. Versions are immutable, so the content
  check (section 7) never fires, and a moved tag is not a conflict.
  `Compare` orders by push time.
- Ingest records an image through `Record`, which takes a package whose
  blobs are stored already: the content check, ownership, placement and
  the publish request are those of an upload.
- Tags are mutable labels on a package, in a new table `tags`
  (`repository_id`, `format`, `name`, `tag`, `package_id`, `updated_at`,
  `updated_by`). npm dist-tags would use the same table. A snapshot records
  the tags it served in `snapshot_tags`, so a rollback restores them.
- A package owns several blobs: its manifest, its config and layers, and
  for an index its child manifests and their blobs. A new table
  `package_blobs` (`package_id`, `sha256`) lists them, and GC and verify
  follow it. `blob_sha256` is the manifest itself. deb and rpm packages
  have no rows there.
- Child manifests of an index are blobs of the index's package, not
  packages of their own. A manifest pushed by digest that no tag reaches is
  collected after `oci.upload_expiry`.
- Blobs stored before a package references them, the layers and child
  manifests of a push or a sync, are kept by `staged_blobs` until
  `oci.upload_expiry`; GC otherwise deletes every unreferenced blob. A
  `HEAD` that lets a pushing client skip a staged layer extends its stage.
- A finished upload is copied into a new blob file, never linked: a
  request still writing to the session can then only make the digest
  check fail, not change a stored blob.
- Layers are stored once in the blob store, whichever images and names use
  them: OCI digests are sha256, the store's key.
- Ownership uses the namespace `oci`. A name's owner owns the whole
  repository path, and `owns:` globs reserve names as for packages. For
  names with `/`, `*` matches `/` too, as the overlap check (section 8)
  already assumes.
- Names with a path component `blobs`, `manifests`, `tags`, `uploads` or
  `referrers` are refused, so that one name's files never sit inside
  another's in the static tree (`a/blobs` against the blobs of `a`).
- OCI has no distributions. It uses one implicit distribution, `default`,
  which no request or configuration names.
- Retention: every tagged package is kept, plus the `oci.retention` newest
  untagged ones per name (default 10; a registry source's own `retention`
  when it sets one, never `defaults.retention`) and pinned ones. Tagged
  packages do not count towards it. Deployments pin
  digests, so a moved `latest` must not break the digest it pointed at
  before.

### Serving

Clients require `/v2/` at the host root. garner's own server answers all of
`/v2/`: manifests and tag lists from the current snapshot's tree, blobs
from the pool, with `Content-Type`, `Docker-Content-Digest` and range
requests set as clients expect. The README's proxy examples forward
`/v2/` to garner. Under systemd the listening socket survives a restart,
so pulls wait rather than fail.

The snapshot tree is laid out so that a proxy could serve pulls itself.
That split, below, is not shipped as a tested proxy configuration:

| Request | Served from |
|---|---|
| `GET /v2/` | garner (authentication check) |
| `GET`/`HEAD /v2/<name>/manifests/<tag or digest>` | `public/`, rendered per snapshot |
| `GET /v2/<name>/tags/list` | `public/`, rendered per snapshot |
| `GET`/`HEAD /v2/<name>/blobs/<digest>` | `public/`, the pool |
| `POST`, `PATCH`, `PUT`, `DELETE` under `/v2/`, and `GET /v2/<name>/blobs/uploads/...` | garner |

```
snapshots/<id>/oci/v2/<name>/manifests/<tag>.<kind>
snapshots/<id>/oci/v2/<name>/manifests/sha256:<hex>.<kind>
snapshots/<id>/oci/v2/<name>/tags/list
snapshots/<id>/oci/v2/<name>/blobs -> (relative) pool/oci
pool/oci/sha256:<hex>              hardlinks into blobs/
public/v2 -> current/oci/v2        fixed link
```

- `<kind>` names the media type (`oci-index`, `oci-manifest`,
  `docker-list`, `docker-manifest`). A static server picks `Content-Type`
  from the file name, and clients reject a manifest served with the wrong
  one. A proxy would try each suffix and map it to its media type (nginx
  `try_files` and `types`), as garner's own server does. Blobs are
  served as `application/octet-stream`.
- Static files carry no `Docker-Content-Digest` header; clients would
  hash what they receive.
- A pushed tag can be pulled after the next publish, which is debounced
  (section 3). `docker push` does not read it back. A `HEAD` on a blob
  also finds blobs a push staged, so layers garner has are not sent
  again; their content is served only once an image using them is
  published. Other stored blobs, such as a held package's, are never
  served under `/v2/`, and a push can neither mount them nor refer to
  them from a manifest: a push may use only blobs it sent, ones staged
  by another push or a sync, and ones of published images.
- `/snapshots/<id>/` is not a registry root, so there is no snapshot
  pinning for images. Digests pin.
- Reading is open, as for deb and rpm (section 11). Blobs are shared
  between names, so any blob can be fetched under any name.
- cosign signatures stored under its tag scheme (`sha256-<hex>.sig`) are
  ordinary tags. The OCI 1.1 referrers API comes later; it can be rendered
  statically too.

### Push

garner implements the push half of the OCI distribution spec.

- Blob uploads: `POST /v2/<name>/blobs/uploads/` starts a session, or
  finishes in one request with `?digest=`, or reuses a stored blob with
  `?mount=<digest>&from=<name>`. `PATCH` appends a chunk, which must start
  where the last ended (`416` otherwise). `PUT ?digest=` finishes, `GET`
  reports progress, `DELETE` cancels.
- A session is a row in a new table `uploads` (`id`, `repository_id`,
  `format`, `name`, `size`, `created_at`, `updated_at`) and a file under
  `uploads/<id>` in storage, so another node can continue it (section 18).
  On finish the digest is checked and the file stored as a blob (section 7,
  step 1).
- `PUT /v2/<name>/manifests/<ref>` parses the manifest and checks its media
  type against the body. Every blob and child manifest it references must
  be stored (`MANIFEST_BLOB_UNKNOWN` otherwise). A `PUT` by tag records the
  package if new and points the tag at it; a `PUT` by digest stores the
  manifest only. Ownership is checked (section 8) and a publish requested.
- Ownership is checked when a session starts too, so a push to another
  owner's name is refused (`DENIED`, naming the owner) before layers are
  sent.
- Limits: each blob `storage.max_package_size`, each manifest 4 MiB.
- `DELETE /v2/<name>/manifests/<tag>` removes a tag; by digest it removes
  the package from future snapshots, like `garner remove`. Both need
  `admin`.
- GC deletes blobs that no package references only after
  `oci.upload_expiry` (default 24 h), and stale sessions then too. Without
  this, GC would delete the layers of a push in progress.

Authentication:

- `docker login -u <token name>` with the secret as password: the API's
  Basic credentials, with the same https and trusted-proxy rule (section
  11). Push needs `upload`.
- `/v2/` uses the distribution spec's token flow. `GET /v2/` without
  credentials, and a push without them, answer `401` with
  `WWW-Authenticate: Bearer realm="<public_url host>/v2/token",service="garner"`
  (a push adds its `scope`). `GET /v2/token` gives a client without
  credentials an anonymous token, which passes `GET /v2/` and nothing
  else. With Basic credentials it checks them under the rule above and
  answers `401` for a bad token, so `docker login` fails; a good one gets
  a token that carries the credentials, checked again on each request,
  so any node can check it (section 18). `/v2/` also takes Basic
  credentials directly.
- Manifests and blobs never ask for credentials, so anonymous pulls
  work. A `Basic` challenge on `GET /v2/` would stop them with Docker's
  classic image store, which refuses a Basic challenge it has no
  credentials for; the containerd image store, skopeo (podman's
  library) and crane tolerate it.

### Sync from registries

```yaml
oci:
  retention: 10               # untagged manifests kept per name
  upload_expiry: 24h

registries:
  - name: ghcr
    url: https://ghcr.io
    username: avesst
    token_env: GHCR_TOKEN
  - name: dockerhub
    url: https://registry-1.docker.io
    auth_hosts: [auth.docker.io]
  - name: gitlab-intern
    url: https://registry.gitlab.example.com
    auth_hosts: [gitlab.example.com]
    token_file: /run/secrets/gitlab-registry

sources:
  - name: tool-image
    registry: ghcr
    image: avesst/tool
    tags: ["v*", "latest"]
    as: tool                  # local name; default the image path
    owns: ["tool"]
```

- A source names a `registry` in place of a `forge`. Polling, `owns`,
  `retention`, `approval`, `min_release_age` and status work as for forge
  sources.
- Each poll lists the image's tags (paginated with `Link`), keeps those
  matching `tags`, and resolves each with `HEAD` on its manifest. Only
  digests garner has not stored are downloaded. A tag that moved upstream
  moves locally; its old digest becomes untagged and retention applies.
- The first run takes every matching tag, so `tags` should be narrow.
- A tag moves only when upstream moves it away from the digest it was
  last applied from (`source_tags`). A rollback, or an admin deleting the
  tag, therefore holds until upstream points the tag at a new digest.
- `min_release_age` runs from when garner first saw a digest under a tag.
  Registries record no push time, and an image's `created` field is often
  fixed (reproducible builds set it to the commit time or 1970). Every
  digest seen under a tag is a candidate (`source_tag_candidates`); a poll
  applies the newest candidate that is old enough, fetched by digest, and
  drops the older ones. A tag that upstream moves more often than the
  minimum age therefore still advances, one aged digest at a time, as a
  forge source applies the newest release that is old enough. A candidate
  first seen after the digest upstream points at now was withdrawn
  upstream and is dropped, as are all candidates when upstream is back on
  the applied digest. A candidate is dropped as gone only when its
  manifest returns 404. The newest waiting candidate shows in status as a
  deferred release. A processed digest is recorded in `seen_releases`.
- Tag state (`source_tags`, `source_tag_candidates`) is kept per local
  name, so changing a source's `as` syncs its images under the new name.
- With `approval: manual` a new digest is held without its tag; once
  approved, the next poll points the tag at it. A held digest is not
  fetched again and its blobs are not staged, so pushes can't use them;
  sync stages only blobs no package owns yet, and unstages a held image's
  blobs once it is recorded.
- Two registry sources may not serve images under one local name.
- A tag deleted upstream stays in garner.
- A processed digest that is no longer placed under the local name
  (retention, a rollback or a delete removed it) is not synced again, as a
  processed forge release is never ingested twice.
- Each registry has one client, shared by its sources and rebuilt on
  reload so rotated tokens apply, so `max_concurrent` holds across the
  sources of one node. A 429 is waited out once when its
  `Retry-After` is at most 2 minutes; a longer limit fails the poll.
- A whole index is copied, all platforms included, so digests and
  signatures stay valid. Filtering platforms would rewrite the index and
  change its digest.
- Every manifest and blob is checked against its digest as it is stored.
- The registry client is local, like the forge clients. It follows the
  `Bearer` challenge to the token endpoint, anonymously or with the
  configured credentials, and sends `Accept` for all four manifest types.
- Credentials go to the registry's own host and to a token endpoint only
  on that host or in `auth_hosts`, never to a host a response names.
  Blob downloads that redirect to another host (ghcr.io and Docker Hub
  redirect to their CDNs) are fetched without credentials, as forge links
  are (section 7).
- `max_concurrent`, `ca_file`, `proxy` and `Retry-After` handling work as
  for forges. Docker Hub limits anonymous pulls per address, so a Docker
  Hub registry should have credentials.

### Format interface

- A format with live routes gets a handler at the host root (`/v2/`); the
  server rejects prefixes that overlap garner's own routes or each other
  (section 17).
- A package can own several blobs. The core links each into
  `pool/<format>/` at a path the format returns per blob, and GC,
  `snapshot_packages.served_sha256` (the manifest digest) and verify cover
  them all.
- The core stores tags and records them per snapshot; the format renders
  them.
- Ingest accepts a parsed package and its blobs from a format's live
  handlers and from sync, so push, sync and upload share the ownership,
  conflict and publish steps. `PUT /api/v1/upload` does not take images.
- Methods about distributions, architectures and client files return
  nothing for oci; `DistributionKey` is empty, so upload ignores it.
- Optional interfaces carry the rest, so deb and rpm are unchanged:
  `BlobPooler` (blobs pooled by digest), `RetentionDefaulter`,
  `RegistryFormat` (delivered by registry sources) and `Lister` (its own
  UI section).

### Verification and UI

- `garner verify` checks that every tag and digest file parses, that its
  digest, suffix and media type agree with the database and the
  snapshot's tags, that `tags/list` lists exactly those tags, and that
  every blob a package references is in the pool with its recorded size.
- The UI has a Container images tab (section 14): how to pull
  and push, with the host taken from `server.public_url`, and counts of
  images, tags, platforms and images per top-level namespace, then every
  image, paged and filterable. Clients need https, or the host in
  their `insecure-registries`.

### Later

- Image upload as an archive (`docker save`, `oci-archive`) through
  `PUT /api/v1/upload`.
- The referrers API.
- cosign verification at sync (section 20).
- Platform filtering at sync, accepting the changed digest.

## 20. Upstream signatures (later)

Not built yet. Today a synced asset is checked against a checksum (section
7, Verification), which catches corruption but not a replaced release: whoever
can change an asset can change its checksum file too. This section designs
checking that a release was signed by a signer the source trusts. It
replaces the earlier plan of a `gpg_fingerprint` key followed by separate
Sigstore and attestation support.

### Background

A release signature takes one of two forms:

- **A pinned key (OpenPGP).** The project signs with a long-lived private
  key; the verifier trusts its public key, as apt trusts a repository key.
  Files: `SHA256SUMS.asc`, `tool.deb.asc`, `*.gpg`, binary `*.sig`.
- **A CI identity (Sigstore, "keyless").** The project's CI job proves its
  identity to an OIDC issuer (GitHub Actions, GitLab), gets a short-lived
  Fulcio certificate naming it (repository, workflow, ref) and signs; the
  signature is recorded in the public Rekor log. The verifier trusts an
  identity, not a key. Anyone can obtain a Sigstore signature, so the
  identity must be pinned. Files: `*.sigstore.json` bundles, which carry
  the certificate and the log proof, so they verify offline.

Either form signs the checksum file (one signature covers every asset in
it) or each asset. GitHub artifact attestations are Sigstore bundles
stored by GitHub and looked up by the file's digest, not release assets.

A survey of 14 projects (November 2026, latest release of each): Sigstore
bundles on the checksum file (goreleaser, cosign, trivy) or on each asset
(opentelemetry-collector-releases, trivy); OpenPGP on the checksum file
(syncthing, restic); GitHub attestations only (cli/cli, grafana/alloy) or
next to the files (goreleaser, cosign, syncthing, trivy); cosign's older
`.sig` with `.pem` (caddy); checksums only (node_exporter, traefik,
lazygit, hugo). Two verifiers, OpenPGP and Sigstore, cover all but the
last two groups.

### Configuration

A source names the signers it trusts, not files:

```yaml
sources:
  - name: tool
    forge: github
    repo: example/tool
    signers:
      - sigstore: {}
      - pgp: { key_file: /etc/garner/keys/tool.asc }
```

- `sigstore: {}` trusts a CI workflow of the synced repository, matched on
  the certificate's OIDC issuer and source repository: on GitHub, issuer
  `https://token.actions.githubusercontent.com` and repository
  `https://github.com/<repo>`; on GitLab, the instance URL and the project
  path. On an `org` or `group` source each repository is its own signer,
  so one entry covers them all. `gh attestation verify --repo` uses the
  same default.
- `sigstore` can narrow or redirect this: `identity`, a glob on the
  certificate's identity (`https://github.com/example/tool/.github/workflows/release.yml@refs/tags/*`,
  for one workflow and tags only); `issuer`; and `repository`, for releases
  built in another repository. A forge without a default issuer
  (Forgejo) needs `issuer` and `identity` set.
- `pgp` trusts the keys in `key_file`. A signature counts if it was made
  while the key was valid; a revocation in the file is honoured.
- `signers` sits in `defaults` and per source, like `verify`, so the
  same validation and precedence apply.

### Rule

- A source without `signers` behaves as today.
- With `signers`, an asset is accepted only if one of these verifies
  against a listed signer: its own signature; the signature of a checksum
  file that lists its hash (the asset still has to match that hash); or,
  on GitHub, an attestation for its digest. Otherwise it is rejected and a
  `signature` conflict is recorded, whatever `verify` says.
- A signature that does not verify is treated as absent; it never rejects
  an asset that another signature accepts. A checksum mismatch still
  rejects it (section 7).
- `verify_status` gains `signed`, above `verified` and `unverified`, and
  the UI and `packages list` show it.

### Discovery

- Only candidate assets and the checksum files that cover them are looked
  at; other assets are never downloaded.
- Signature files are found by name next to the file they sign:
  `<file>.sigstore.json`, `<file>.sigstore`, `<file>.asc`, `<file>.gpg` and
  `<file>.sig`. A `.sig` is classified by content, since OpenPGP and
  cosign both use the name. A `signatures:` list of globs, like
  `checksums:`, replaces the built-in names.
- On GitHub, with a `sigstore` signer, one attestation lookup per asset
  digest (`GET /repos/<repo>/attestations/sha256:<hex>`). Lookups count
  against the API rate limit, so such sources should have a token.
- A signature file that fails to download is retried like a checksum file
  (section 7).
- A signed checksum file is read even when the forge gives digests, unlike
  an unsigned one (section 7), since its signature is what counts.

### Sigstore trust root

Verifying Sigstore needs the trusted root: Fulcio's certificate
authorities and the Rekor and timestamp authority keys. By default garner
keeps it current through Sigstore's TUF repository
(`tuf-repo-cdn.sigstore.dev`), cached under `storage.path`; a
`sigstore.trusted_root` file replaces that for hosts without that access,
and has to be updated by hand when Sigstore rotates keys. Verification
itself is offline: bundles carry their log proofs, and Rekor is never
queried during a sync.

### Implementation

- Format-agnostic, in `internal/sync`, next to checksum verification.
- OpenPGP uses `ProtonMail/go-crypto`, already a dependency.
- Sigstore uses `github.com/sigstore/sigstore-go` rather than a local
  implementation: verification spans certificate chains, transparency log
  proofs and RFC 3161 timestamps, and a missed check accepts forgeries.
  It is a large module (about 360 modules in its graph) and its v1.4
  needs Go 1.27; it must be approved before slice 2 (docs/plan.md,
  Dependencies).
- Tests use real bundles and signatures as test data, checked against a
  known digest, so the packages themselves are not needed; each check
  (identity, issuer, digest, log proof, trust root) is broken once.

### Slices

1. OpenPGP signatures on checksum files and assets; `signers`, the rule,
   discovery by name, `signed` status.
2. Sigstore bundles in release assets, and the trust root.
3. GitHub artifact attestations, reusing slice 2's verifier.

### Not covered

- cosign's older `.sig` with `.pem` output, which carries no log proof and
  would need a Rekor query during sync.
- minisign and signify, used by none of the surveyed projects.
- Signatures embedded in `.rpm` files.
- cosign signatures on synced OCI images (section 19, Later); they would
  reuse the Sigstore verifier but are found through the registry.
