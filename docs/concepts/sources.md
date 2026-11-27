# Sources

A source is something garner watches for new releases and publishes on
its own, so nobody has to upload them. There are two kinds:

- a **forge source**: one repository, a GitHub or Forgejo organization, or
  a GitLab group, whose release assets garner takes `.deb` and `.rpm`
  files from. See [forges](forges.md).
- a **registry source**: one image on a registry such as ghcr.io or Docker
  Hub, whose tags garner copies. See [registries](registries.md).

Both kinds deliver through the same [ingest](ingest.md) as uploads, so
ownership, conflicts and retention work the same way.

## How it works

### Polling

garner polls each source every hour by default (`poll_interval`), with
10 % of random jitter so sources don't all poll at once. A new source, at
start or after a reload, polls for the first time within 30 seconds.
`garner sync <source>` polls it at once.

Polls are cheap when nothing changed: garner sends the forge's ETags back,
and the forge answers "not modified".

A reload restarts only the sources whose settings changed, including the
defaults they inherit and their forge's or registry's settings. Others
keep their schedule. A sync in progress on a changed or removed source is
stopped, and the next run picks up where it left off.

### Which releases a forge source takes

- **The first run** takes only the latest release of each repository that
  has assets garner can use, so adding a source does not import years of
  history.
- **Later runs** take every release published since the last one
  processed, in the order the forge published them.
- **Drafts** are never taken. **Pre-releases** are skipped unless
  `prereleases: true`.
- **A release without package assets yet** is not marked done: CI
  pipelines often create a release first and attach files afterwards.
  garner waits for its assets until a newer release appears.
- **A release that was processed** is never processed again, even if its
  date is edited. Assets attached to it later are not picked up.
- **A release deleted upstream** leaves its packages in garner; retention
  removes them in time.

For registry sources, each poll lists the image's tags, keeps those that
match `tags`, and copies the digests garner does not have yet. See
[registries](registries.md).

### Waiting before publishing: `min_release_age`

With `min_release_age` set, a source processes a release only once it is
that old, for example `336h` for two weeks. If a release is compromised
and upstream pulls it during the wait, garner never publishes it.

- A younger release is **deferred**: `garner status` lists it with the
  time it becomes eligible, and the metric
  `garner_sync_releases_deferred` counts it.
- A release that upstream pulls or turns into a draft during the wait is
  dropped.
- On its first run, a source takes the newest release that is already old
  enough; newer ones follow as they age.
- For an urgent fix, `garner sync <source> --ignore-age <tag>` lets one
  release through. It needs both the `publish` and `admin` scopes, and is
  logged with the token's name.

Uploads and pushes are never delayed.

### Checking what is downloaded

Every asset is checked against a checksum when one exists: the digest
the forge reports for the asset, or a checksum file in the release
(`checksums.txt`, `SHA256SUMS`, `*.sha256` and similar, or the names in
`checksums:`). A mismatch rejects the asset.

- `verify: opportunistic`, the default, accepts an asset without any
  checksum and marks it unverified.
- `verify: required` rejects it.

Registry images are always checked: every manifest and layer is named by
its digest.

### Approval

With `approval: manual`, everything the source delivers is stored but
**held** until `garner approve`. See
[ingest](ingest.md#manual-approval).

### Settings and defaults

`retention`, `poll_interval`, `verify`, `approval`, `prereleases` and
`min_release_age` can be set in `defaults` for every source, and
overridden per source.

## Why it works this way

- **Only new releases.** Starting from the latest release keeps a new
  source from flooding the repository, and following publish order rather
  than comparing tags works for projects whose tags are not versions
  (`nightly`, `2026-05-release`).
- **Releases are processed once.** A repository changes only by new
  releases. A file that changed under an existing release would be a
  different package under the same version, which hosts that installed
  the first would never pick up.
- **Polling.** It needs no setup on the forge side and works through
  firewalls, and ETags keep it cheap.

## Limits

- garner has no webhook endpoint: a release appears after the next poll,
  or at once with `garner sync <source>`.
- Upstream signatures (OpenPGP, Sigstore, GitHub attestations) are not
  checked yet, only checksums. A checksum protects against corruption,
  not against a release replaced by someone who can also replace its
  checksum file. `min_release_age` reduces that risk.
- A source's packages are owned by the source. Reserve its names with
  `owns:` so an upload cannot claim them first; see
  [ownership](ownership.md#reserving-names-for-a-source).

## See also

- [Forges](forges.md), [registries](registries.md)
- [Syncing sources](../guides/sync-sources.md): a guide to setting them up
- Configuration: [`sources`](../reference/configuration.md#sources),
  [`defaults`](../reference/configuration.md#defaults),
  [minimum release age](../reference/configuration.md#minimum-release-age)
