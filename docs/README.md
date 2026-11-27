# garner documentation

garner runs signed apt and dnf repositories and a container registry, and
fills them from uploads, image pushes, and releases synced from forges and
registries. These pages explain how it works and how to run it.

## Where to start

- **New to garner:** read the [overview](concepts/overview.md), then
  follow [Getting started](getting-started.md).
- **Running garner:** the [concept pages](#how-garner-works) explain each
  part; the [guides](#guides) cover the tasks.
- **Delivering packages from CI:** [CI uploads](guides/ci-uploads.md),
  [container images](guides/container-images.md), and
  [name ownership](concepts/ownership.md).
- **Installing from a garner repository:** open the repository's own page
  in a browser. It shows the client files and commands for apt, dnf and
  docker.

## How garner works

- [Overview](concepts/overview.md): the whole picture in one page
- [Ingest](concepts/ingest.md): what happens to a package or image on its
  way in
- [Name ownership](concepts/ownership.md)
- [Publishing and snapshots](concepts/publishing.md)
- [Retention and garbage collection](concepts/retention-gc.md)
- [Signing](concepts/signing.md)
- [Sources](concepts/sources.md), [forges](concepts/forges.md) and
  [registries](concepts/registries.md)
- [Security and tokens](concepts/security.md)
- [Storage](concepts/storage.md)
- [Verification](concepts/verification.md)
- Formats: [deb](concepts/formats/deb.md), [rpm](concepts/formats/rpm.md),
  [container images](concepts/formats/oci.md)

## Guides

- [Getting started](getting-started.md): from install to `apt install`
- [Uploading packages from CI](guides/ci-uploads.md)
- [Syncing releases from forges](guides/sync-sources.md)
- [Container images](guides/container-images.md): push, pull and sync
- [Rotating the signing key](guides/key-rotation.md)
- [Backup and restore](guides/backup-restore.md)
- [Upgrading garner](guides/upgrades.md)
- [Monitoring](guides/monitoring.md)
- [Troubleshooting](guides/troubleshooting.md)

Installing garner and putting it behind a reverse proxy are in the
[README](../README.md#install).

## Reference

- [Configuration](reference/configuration.md): every key, its default and its
  validation
- [HTTP API](reference/api.md)
- `garner(1)` and `garner.yaml(5)`: the man pages, installed with the
  `.deb` and `.rpm` packages (`man garner`, `man garner.yaml`)

## For contributors

- [Design](dev/design.md): the specification, with the reasons behind each
  decision
- [Plan](dev/plan.md): what is built, and what is verified and how
