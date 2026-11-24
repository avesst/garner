# Configuration reference

garner reads one YAML file. It holds the decisions you make: what to serve,
where to sync from and how to sign. Everything garner learns while running
(packages, tokens, owners, snapshots) lives in the database instead.

- Default path: `/etc/garner/garner.yaml`. Override it with `--config`.
- Check a file with `garner config validate <file>`. It works offline and
  never opens the files that `*_file` keys point to, so it can run in CI.
- Reload a running server with `SIGHUP` or `garner reload`. An invalid file
  is rejected and logged, and the last good configuration stays active.
- Unknown keys are errors, so a misspelled key fails validation instead of
  being ignored. The file must hold exactly one YAML document.
- Validation reports every problem at once, each prefixed with its key path,
  for example `sources[2].assets[0].suite: unknown suite "noble"`.
- Secrets are never written inline. Point to them with a `*_file` key or a
  `*_env` key.

Every key is optional unless marked **required**. A key you leave out keeps
its default. The format sections, `deb`, `rpm` and `oci`, are the exception: a
format is enabled by its section being present, and at least one is
required. An empty section (`deb:` or `deb: {}`) enables the format with
the defaults below.

## Value formats

| Format | Written as | Examples |
|---|---|---|
| Duration | A number with a unit: `s`, `m`, `h`. There is no unit for days. | `30m`, `1h`, `1h30m`, `168h` |
| Size | Bytes, or a number with a unit: `KB` `MB` `GB` `TB` (powers of 1000) or `KiB` `MiB` `GiB` `TiB` (powers of 1024) | `2GiB`, `500MB`, `1048576` |
| Glob | `*` matches any run of characters, `?` one character, `[...]` a character class (`[^...]` negates it), `\` escapes the next character | `tool-*`, `*_amd64.deb` |
| Listen address | `host:port` or `:port` | `:8080`, `127.0.0.1:9090` |

## `server`

| Key | Type | Default | Description |
|---|---|---|---|
| `listen` | listen address | `:8080` | Where the HTTP server listens. Changing it needs a restart. |
| `public_url` | URL, **required** | none | The URL clients use. It goes into the apt and dnf client files and the UI, and its host is the registry host that image names start with. Must be an absolute `http` or `https` URL without a query or fragment. A path is allowed, for example `https://example.com/garner/`. |
| `client_name` | name | `garner` | Prefix of the files clients install: `<name>.sources`, `<name>.repo`, `/usr/share/keyrings/<name>.gpg`, and the dnf repo ID. Lowercase letters, digits, `.`, `_` and `-`; must start with a letter or digit. |
| `trusted_proxies` | list of IPs or CIDR prefixes | `[]` | Reverse proxies whose `X-Forwarded-*` headers are honoured. Authenticated requests are accepted only from loopback, or from one of these with `X-Forwarded-Proto: https`. Others get `403`. |
| `allow_insecure_auth` | bool | `false` | Accept authenticated requests over plain HTTP from any address. Tokens then travel unencrypted, so use this only on an isolated lab network. |

## `metrics`

| Key | Type | Default | Description |
|---|---|---|---|
| `listen` | listen address | empty | Serve `/metrics` on a separate address. When empty, `/metrics` is served on `server.listen`. Changing it needs a restart. |

## `log`

| Key | Type | Default | Description |
|---|---|---|---|
| `format` | `json` or `text` | `json` | Log line format. |
| `level` | `debug`, `info`, `warn` or `error` | `info` | Lowest level that is logged. |

## `database`

| Key | Type | Default | Description |
|---|---|---|---|
| `url` | URL | `sqlite:///var/lib/garner/garner.db` | The database. Only SQLite is supported: `sqlite://` followed by an absolute path, so three slashes in total. Keep the file on local disk, never on NFS. Changing it needs a restart. |

## `storage`

| Key | Type | Default | Description |
|---|---|---|---|
| `path` | absolute path | `/var/lib/garner` | Root of package blobs, snapshots and `public/`, the directory clients are served from. Changing it needs a restart. |
| `gc_grace` | duration | `10m` | How long a replaced snapshot is kept before deletion, so requests already resolved through it can finish. Must not be negative. |
| `index_history` | duration | `1h` | Index files of every snapshot published within this window are carried into each new snapshot, so a client that fetched the signed top-level file just before a publish can still fetch the indexes it lists. The previous snapshot's index files are always carried over. Must not be negative. |
| `snapshot_retention` | integer | `30` | How many snapshots are kept, newest first. The current snapshot is always kept. Must be at least 2. |
| `max_package_size` | size | `2GiB` | Largest package garner accepts, from an upload or as a synced release asset. A larger upload gets `413`; a larger asset is skipped and recorded as a conflict. Must be greater than zero. |

## `signing`

| Key | Type | Default | Description |
|---|---|---|---|
| `keys` | list of keys | `[]` | OpenPGP keys. All of them are published in `/key.asc` and `/key.gpg`; the first one signs. When the list is empty, `garner init` generates an RSA 4096 key at `<storage.path>/signing/key.asc` and garner uses it. |

Each entry of `keys`:

| Key | Type | Default | Description |
|---|---|---|---|
| `key_file` | path, **required** | none | Armored private key. |
| `passphrase_file` | path | empty | File holding the key's passphrase. Leave it empty for a key without a passphrase, which is the default for unattended signing. |

To rotate keys:

1. Add the new key second.
2. Wait for clients to fetch the keyring.
3. Move the new key first.
4. Remove the old key.

Promoting a new key re-signs every rpm package from its original.

## `deb`

Present: garner serves an apt repository. Absent: deb is disabled.

| Key | Type | Default | Description |
|---|---|---|---|
| `architectures` | list | `[amd64]` | Debian architectures to publish. Lowercase letters, digits and `-`. Don't list `all`: architecture-independent packages are published under every listed architecture automatically. |
| `suites` | list | `[stable]` | Suites to publish. Letters, digits, `.`, `_` and `-`. The first suite, or `stable` if listed, is the default: uploads without `--suite` and sources without asset rules use it, and its client file is served at the root, the others below `clients/<suite>/`. |

## `rpm`

Present: garner serves dnf repositories. Absent: rpm is disabled.

rpm also needs `createrepo_c`, `rpmsign`, `gpg` and `gpgconf` on `PATH`. With
the section present and a tool missing, `serve` refuses to start and names
the missing tools.

Removing a format's section keeps its packages stored, but no longer serves
them, and `serve` logs a warning about them at every start; adding the
section back serves them again, re-signed if the key changed meanwhile. Uploads and synced assets
of a disabled format are rejected.

| Key | Type | Default | Description |
|---|---|---|---|
| `architectures` | list | `[x86_64]` | rpm architectures to publish. Lowercase letters, digits and `_`. Don't list `noarch`; it is implicit, as `all` is for deb. `i686` is multilib: its packages are listed in the x86_64 repository, where dnf on x86_64 hosts finds them, so it requires `x86_64` and gets no repository of its own. |
| `releases` | list | `[stable]` | Releases to publish. Same rules and default handling as `deb.suites`. |

## `oci`

Present: garner is a container registry at `/v2/` on the host root, which
`docker push`, `docker pull` and other OCI clients use. Absent: images are
disabled. Images are pushed or synced from registries, never uploaded with
`garner upload`. Pushes need a token with the `upload` scope, used with
`docker login <host> -u <token name>` and the token's secret as the
password.

| Key | Type | Default | Description |
|---|---|---|---|
| `retention` | integer | `10` | Untagged manifests kept per image name, newest first, besides every tagged one. A manifest becomes untagged when its tag moves to a newer push; deployments that pin digests keep working until it ages out. `0` keeps all. Also applies to images from a registry source that does not set its own `retention`; `defaults.retention` does not apply to images. |
| `upload_expiry` | duration | `24h` | How long a pushed blob that no manifest uses yet, and an idle upload session, are kept before GC removes them. Must be at least `1m`. |

## `defaults`

Settings for every source that does not override them. `retention` also
applies to uploaded packages.

| Key | Type | Default | Description |
|---|---|---|---|
| `retention` | integer | `3` | Versions kept per package name, distribution and architecture. Older versions leave future snapshots; pinned packages are always kept. Must be at least 1. |
| `poll_interval` | duration | `1h` | How often a source is polled, with ±10 % jitter. Must be at least `1m`. |
| `verify` | `opportunistic` or `required` | `opportunistic` | Checksum policy. An asset is checked against the forge's digest or the release's checksum file when one exists, and a mismatch always rejects it. `opportunistic` accepts assets without a checksum and marks them unverified. `required` rejects them. |
| `approval` | `auto` or `manual` | `auto` | With `manual`, new packages are stored as held and published only after `garner approve`. |
| `prereleases` | bool | `false` | Sync pre-releases too. Drafts are never synced. |
| `min_release_age` | duration | `0s` | How old a synced release and its assets must be before garner processes it, so a compromised release has time to be noticed and pulled first. `0s` turns the wait off. Must not be negative. See [Minimum release age](#minimum-release-age). |

## `forges`

A list of forges that sources sync from. There are none by default.

| Key | Type | Default | Description |
|---|---|---|---|
| `name` | name, **required** | none | Name that sources refer to. Lowercase letters, digits, `_` and `-`; must start with a letter or digit; unique. |
| `type` | **required** | none | `github`, `forgejo`, `gitea` (an alias of `forgejo`) or `gitlab`. |
| `url` | https URL | empty | The forge's base URL. Required except for `github`, where empty means github.com; set it to use GitHub Enterprise Server. Must be `https`, because tokens are sent to it. |
| `token_env` | variable name | empty | Environment variable holding the API token. |
| `token_file` | path | empty | File holding the API token; surrounding whitespace is trimmed. Set `token_env` or `token_file`, not both. With neither, requests are unauthenticated. |
| `ca_file` | path | empty | PEM file with the CA certificates that sign the forge's TLS certificate. TLS verification can't be turned off. |
| `max_concurrent` | integer | `4` | Most requests in flight to this forge at once. Must be at least 1. |
| `proxy` | URL | empty | Proxy for this forge's requests: an `http`, `https` or `socks5` URL. When empty, `HTTPS_PROXY` and `NO_PROXY` from the environment apply. |

The token is sent only to the forge's own scheme, host and port. Asset links
that point elsewhere are fetched without it.
For a private repository, set `url` to the address the forge puts in its
asset links (Forgejo's `ROOT_URL`, GitLab's `external_url`); otherwise the
downloads fail without the token and are retried on every poll.

## `registries`

A list of OCI registries that registry sources sync images from. There are
none by default.

| Key | Type | Default | Description |
|---|---|---|---|
| `name` | name, **required** | none | Name that sources refer to. Same rules as forge names. |
| `url` | https URL, **required** | none | The registry's address without a path, such as `https://ghcr.io` or, for Docker Hub, `https://registry-1.docker.io`. Must be `https`. |
| `username` | string | empty | User name sent with the token. Needs `token_env` or `token_file`. |
| `token_env` | variable name | empty | Environment variable holding the password or token. |
| `token_file` | path | empty | File holding the password or token; surrounding whitespace is trimmed. Set `token_env` or `token_file`, not both. With neither, garner pulls anonymously. |
| `auth_hosts` | list of hosts | `[]` | Hosts besides the registry's own that may receive the credentials as token endpoints: `auth.docker.io` for Docker Hub, the GitLab host for a GitLab registry. ghcr.io needs none. |
| `ca_file` | path | empty | As for forges. |
| `max_concurrent` | integer | `4` | As for forges. |
| `proxy` | URL | empty | As for forges. |

Credentials go only to the registry's host and to a token endpoint on it or
in `auth_hosts`. A registry that names another token endpoint gets an
error, not the credentials. Blob downloads redirected to other hosts, as
ghcr.io and Docker Hub do to their CDNs, are fetched without credentials.
Docker Hub limits anonymous pulls per address, so give a Docker Hub
registry credentials.

## `sources`

A list of places to sync from. There are none by default. A forge source
is one repository (`repo`), a GitHub or Forgejo organization (`org`), or a
GitLab group (`group`); set exactly one of the three. A registry source is
one image of a registry; see [Registry sources](#registry-sources).

| Key | Type | Default | Description |
|---|---|---|---|
| `name` | name, **required** | none | Unique name, used in logs, metrics, status and `garner sync <name>`. Same rules as forge names. |
| `forge` | forge name, **required** | none | The `forges[].name` to sync from. |
| `repo` | `owner/name` | empty | A single repository. GitLab paths may have more parts (`group/sub/project`). |
| `org` | name | empty | A GitHub or Forgejo organization; every repository in it is synced. New repositories are picked up on the next poll. |
| `group` | path | empty | A GitLab group, synced like `org`. |
| `owns` | list of globs | `[]` | Package names this source reserves. Only this source may deliver them, also against uploads. Globs of two sources must not be able to match the same name; validation names an example if they do. |
| `assets` | list of rules | `[]` | Which deb suite or rpm release each asset goes to; see [Asset rules](#asset-rules). |
| `include` | list of globs | `[]` | For `org` and `group` sources only: sync only repositories whose name (without the owner) matches one of these. Empty means all. |
| `exclude` | list of globs | `[]` | Skip repositories whose name matches, and skip assets whose file name matches. Matching assets are never downloaded. |
| `checksums` | list of globs | `[]` | File names of a release's checksum files. They replace the built-in names (`checksums.txt`, `*_checksums.txt`, `SHA256SUMS`, `*.sha256`). Checksum files are downloaded only for package assets without a forge digest, and one named after another asset (`tool.deb.sha256`) only for that asset. |
| `archived` | bool | `false` | Also sync archived repositories of an `org` or `group`. |
| `forks` | bool | `false` | Also sync forks in an `org` or `group`. |
| `recursive` | bool | `true` | Walk GitLab subgroups. Only valid on GitLab `group` sources. |

A source can also set `retention`, `poll_interval`, `verify`, `approval`,
`prereleases` and `min_release_age`, which override [`defaults`](#defaults)
for that source only. The same rules apply.

Some asset names are always skipped: `*.src.rpm`, `*-dbgsym*` and
`*-debuginfo*`, and files without the extension of an enabled format
(`.deb`, `.rpm`).

On its first run, a source syncs only the latest release of each repository
(with `min_release_age`, the latest that is old enough). Later runs pick up
every release published since then.

### Registry sources

A registry source sets `registry` in place of `forge`:

| Key | Type | Default | Description |
|---|---|---|---|
| `registry` | registry name, **required** | none | The `registries[].name` to sync from. |
| `image` | image name, **required** | none | The image's path on the registry, such as `avesst/tool` or, on Docker Hub, `library/alpine`. |
| `tags` | list of globs, **required** | none | Tags to sync. Every matching tag is synced on the first run, so keep them narrow (`v*`, `latest`). |
| `as` | image name | `image` | The name the image is served under. |

`owns`, `retention`, `poll_interval`, `approval` and `min_release_age`
work as for forge sources; `repo`, `org`, `group`, `assets`, `include`,
`exclude`, `checksums`, `archived`, `forks` and `recursive` are not
allowed. A registry source's `owns` reserves image names, and a forge
source's reserves package names, so the same glob in both is no
conflict.

Each poll lists the image's tags and resolves the matching ones. A digest
garner has not stored is downloaded whole, every platform of an index
included, and every blob is checked against its digest. A tag that moves
upstream moves in garner too; the digest it pointed at becomes untagged
and is kept per `retention`. A tag deleted upstream stays in garner. A
digest garner has processed is never synced again once it is removed,
whether by retention, `garner snapshot rollback` or a delete, so a
rollback holds until the tag moves upstream to a new digest.

With `min_release_age`, the wait runs from when garner first saw the
digest under the tag: registries record no push time, and an image's own
creation time is often fixed. Each poll applies the newest digest seen
under the tag that is old enough, so a tag that upstream moves more often
than the minimum age still advances, lagging by about that age. A digest
upstream withdraws, by moving the tag back to an earlier one before it is
old enough, is dropped and never applied.
`garner sync <source> --ignore-age <tag>` lets the waiting digests of a
tag through.

A tag moves only when upstream moves it. After `garner snapshot
rollback`, or after an admin deletes the tag, it stays as it is until
upstream points it at a new digest.

Two registry sources may not serve images under the same name. Changing
a source's `as` syncs its images under the new name on the next poll;
the old name keeps its images and tags until you remove them with
`garner remove`.

With `approval: manual`, a new digest is held without its tag; after
`garner approve`, the next poll points the tag at it.

### Asset rules

Each entry of `assets`:

| Key | Type | Default | Description |
|---|---|---|---|
| `match` | glob, **required** | none | Asset file names this rule applies to. |
| `suite` | suite name | empty | Target for `.deb` assets. Must be listed in `deb.suites`, so `deb` must be configured. |
| `release` | release name | empty | Target for `.rpm` assets. Must be listed in `rpm.releases`, so `rpm` must be configured. |

Set `suite`, `release` or both. Rules are evaluated in order. The first rule
whose `match` fits the asset name and that sets a target for the asset's
format wins, so a `*` rule with only `suite` does not catch rpm assets.

- With no rules at all, every asset goes to the default suite or release:
  `stable` if listed, else the first one.
- With rules, an asset that no rule places is skipped and not downloaded.
- If two assets of one release end up as the same package, version,
  architecture and distribution, both are skipped and a conflict is
  recorded.

### Minimum release age

With `min_release_age` set, a release is processed only once it is that old.
The age runs from the release's publish time or, on GitHub and Forgejo,
from the upload of its newest package asset if that is later. On GitLab,
a release backdated with a past `released_at` is measured from when it was
created. For two weeks, write `336h`.

- A younger release is deferred. Later polls check it again, and
  `garner status` lists it with the time it becomes eligible. It is logged
  once, as `deferring release`, when first deferred; each run's
  `sync finished` line counts deferred releases, and so does the metric
  `garner_sync_releases_deferred`, per source.
- On its first run, a source takes the latest release that is old enough;
  newer releases follow as each one ages. A new source can therefore
  publish several recent versions over its first days; `retention` trims
  them.
- A release whose assets are attached after publishing waits for them, as
  without the gate, and then for their age. On GitLab, whose release links
  carry no time, an old-enough release is processed as it is: links added
  later are not picked up, so a pipeline must attach them within
  `min_release_age`.
- A release without a publish time waits until it is let through by hand.
- Uploads are not affected.

For an urgent fix, let one release through:

```sh
garner sync tool --ignore-age v1.2.4
garner sync team --ignore-age v1.2.4 --repo team/tool   # org or group source
```

This needs a token with both the `publish` and `admin` scopes, and a source
with `min_release_age` set. It covers
only a release, and package assets, uploaded before the command runs: a
release deleted and re-created under the same tag, or an asset uploaded
afterwards, waits again. Run it once the release's assets are attached.
For an org or group source, `--repo` must name a repository the source's
last sync listed. A tag that was already processed is refused, unless a
release under it is waiting (re-created upstream): assets attached to a
processed release are never picked up, so publish a new release. garner
logs a warning when an override has no effect: the release is older than
where the source starts, or it or an asset changed after the override. The decision is logged and
kept in the database with the token's name.

## Changes that need a restart

A reload applies everything except these keys. Changing them makes the reload
fail with a message naming the keys, and the running configuration stays
active until you restart:

- `server.listen`
- `metrics.listen`
- `database.url`
- `storage.path`
- adding or removing a format section (`deb`, `rpm`, `oci`)

A reload restarts only the sources whose effective settings changed,
including inherited defaults and their forge's or registry's settings. The others keep
their schedule.

## Example

A small setup: one internal Forgejo, one repository with separate Ubuntu and
EL9 builds, and a whole organization synced to `stable`.

```yaml
server:
  public_url: https://packages.example.com
  trusted_proxies: ["10.0.0.5"]

storage:
  path: /var/lib/garner

deb:
  architectures: [amd64, arm64]
  suites: [stable, noble]

rpm:
  architectures: [x86_64]
  releases: [stable, el9]

forges:
  - name: forgejo
    type: forgejo
    url: https://git.example.com
    token_file: /run/credentials/garner.service/forgejo-token

sources:
  - name: tool
    forge: forgejo
    repo: platform/tool
    owns: ["tool", "tool-*"]
    assets:
      - match: "*_ubuntu24.04*.deb"
        suite: noble
      - match: "*.el9.*.rpm"
        release: el9
      - match: "*"
        suite: stable
        release: stable

  - name: platform
    forge: forgejo
    org: platform
    exclude: ["legacy-*", "*-musl*"]
    approval: manual
```

`contrib/garner.yaml` is a starting file for a new installation.
