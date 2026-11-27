# Syncing releases from forges

This guide sets up garner to publish the `.deb` and `.rpm` assets of
releases on GitHub, GitLab or Forgejo by itself. For images from
registries, see [container images](container-images.md#sync-images-from-another-registry).
How sync works is in [sources](../concepts/sources.md) and
[forges](../concepts/forges.md).

## 1. Add the forge

In `/etc/garner/garner.yaml`:

```yaml
forges:
  - name: github
    type: github
    token_file: /run/credentials/garner.service/github
```

A token is optional for public repositories, but on GitHub it lifts the
limit from 60 API requests an hour to 5,000; 60 is enough for a few
repositories, not for an organization. Give it read-only access.

The token reaches garner through systemd: store it root-only, say in
`/etc/garner/secrets/github.token`, and add a drop-in with
`sudo systemctl edit garner`:

```ini
[Service]
LoadCredential=github:/etc/garner/secrets/github.token
```

A new `LoadCredential=` takes effect at the next restart, not a reload.
[Secrets](../concepts/security.md#secrets) explains the setup and how to
rotate a token.

For GitLab or Forgejo, set `type` and `url`:

```yaml
  - name: gitlab
    type: gitlab
    url: https://gitlab.example.com
    token_file: /run/credentials/garner.service/gitlab
```

## 2. Add a source

The simplest source is one repository whose release assets all go to the
default suite and release:

```yaml
sources:
  - name: tool
    forge: github
    repo: example-org/tool
    owns: ["tool", "tool-*"]
```

`owns` reserves the package names, so only this source can deliver them.
Add it before anyone uploads a package of that name.

### Send assets to the right suite or release

When a project builds per OS release, map assets by file name:

```yaml
    assets:
      - match: "*_ubuntu24.04*.deb"
        suite: noble
      - match: "*.el9.*.rpm"
        release: el9
      - match: "*"
        suite: stable
        release: stable
```

The first rule that matches the name and sets a target for the asset's
format wins. With rules, an asset that no rule places is skipped and never
downloaded. Every suite and release named must be configured in `deb:` or
`rpm:`.

### Sync a whole organization or group

```yaml
  - name: platform
    forge: gitlab
    group: platform
    include: ["svc-*"]
    exclude: ["svc-sandbox-*"]
    approval: manual
```

Every repository in the group (and its subgroups) is synced, and new ones
are picked up on the next poll. `approval: manual` holds new packages
until `garner approve`, which suits a group whose projects you don't all
know.

### Wait before publishing

```yaml
    min_release_age: 336h   # two weeks
```

garner then publishes a release only once it is that old, so a release
that turns out to be compromised and is pulled upstream never reaches your
hosts. Set it in `defaults` to apply it to every source.

## 3. Apply and check

```sh
garner config validate /etc/garner/garner.yaml
sudo systemctl reload garner
```

A new source polls for the first time within 30 seconds. Then:

```sh
garner status
```

shows each source's last run, last success and last error, and releases
waiting for `min_release_age`. `garner status --json` adds, per source,
the repositories without matching assets (`repos_without_assets`) and
the assets skipped for an unconfigured suite, release or architecture
(`skipped_assets`). `garner packages list --name tool` shows what
arrived.

To poll at once, without waiting for the interval:

```sh
garner sync tool
```

## Common adjustments

| Want | Set |
|---|---|
| Pre-releases too | `prereleases: true` |
| Reject assets without a checksum | `verify: required` |
| The project's checksum file has an unusual name | `checksums: ["tool_*_SHA256SUMS"]` |
| Keep more versions | `retention: 10` |
| Poll more often | `poll_interval: 15m` |
| Skip some assets entirely | `exclude: ["*-musl*"]` |
| Let one release skip the wait | `garner sync tool --ignore-age v1.2.4` (needs `publish` and `admin`) |

## When nothing arrives

- **The repository is in `repos_without_assets`** in
  `garner status --json`: no asset matched. Check the asset names against the
  rules and the built-in exclusions (`*.src.rpm`, `*-dbgsym*`,
  `*-debuginfo*`).
- **The asset is in `skipped_assets`**: the package's architecture, or its target
  suite or release, is not configured.
- **A deferred release:** `min_release_age` is waiting; status shows when
  it becomes eligible.
- **A held package** in `garner packages list`: an ownership conflict, or
  `approval: manual`. See [troubleshooting](troubleshooting.md#held-packages).
- **Only the latest release arrived:** that is intended. A new source
  starts from the latest release; older ones are not imported.

## See also

- [Sources](../concepts/sources.md), [forges](../concepts/forges.md)
- Configuration: [`forges`](../reference/configuration.md#forges),
  [`sources`](../reference/configuration.md#sources),
  [asset rules](../reference/configuration.md#asset-rules)
