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
its default.

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
| `public_url` | URL, **required** | none | The URL apt and dnf clients use. It goes into the client files and the landing page. Must be an absolute `http` or `https` URL without a query or fragment. A path is allowed, for example `https://example.com/garner/`. |
| `client_name` | name | `garner` | Prefix of the files clients install: `<name>.sources`, `<name>.repo`, `/usr/share/keyrings/<name>.gpg`, and the dnf repo ID. Lowercase letters, digits, `.`, `_` and `-`; must start with a letter or digit. |
| `trusted_proxies` | list of IPs or CIDR prefixes | `[]` | Reverse proxies whose `X-Forwarded-*` headers are honoured. Authenticated requests are accepted only from loopback, or from one of these with `X-Forwarded-Proto: https`. Others get `403`. |
| `allow_insecure_auth` | bool | `false` | Accept authenticated requests over plain HTTP from any address. Tokens then travel unencrypted, so use this only on an isolated lab network. |
| `max_upload_size` | size | `2GiB` | Largest package accepted by the upload API. Must be greater than zero. |

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

| Key | Type | Default | Description |
|---|---|---|---|
| `architectures` | list | `[amd64]` | Debian architectures to publish. Lowercase letters, digits and `-`. Don't list `all`: architecture-independent packages are published under every listed architecture automatically. |
| `suites` | list | `[stable]` | Suites to publish. Letters, digits, `.`, `_` and `-`. The first suite, or `stable` if listed, is the default: its client file is served at the root and the others below `clients/<suite>/`. |

## `rpm`

rpm support also needs `createrepo_c`, `rpmsign`, `gpg` and `gpgconf` on
`PATH`. Without them `serve` disables rpm and logs which tools are missing.

| Key | Type | Default | Description |
|---|---|---|---|
| `architectures` | list | `[x86_64]` | rpm architectures to publish. Lowercase letters, digits and `_`. Don't list `noarch`; it is implicit, as `all` is for deb. |
| `releases` | list | `[stable]` | Releases to publish. Same rules and default handling as `deb.suites`. |

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

## `sources`

A list of places to sync releases from. There are none by default. A source
is one repository (`repo`), a GitHub or Forgejo organization (`org`), or a
GitLab group (`group`). Set exactly one of the three.

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
| `checksums` | list of globs | `[]` | File names of a release's checksum files. They replace the built-in names (`checksums.txt`, `*_checksums.txt`, `SHA256SUMS`, `*.sha256`). |
| `archived` | bool | `false` | Also sync archived repositories of an `org` or `group`. |
| `forks` | bool | `false` | Also sync forks in an `org` or `group`. |
| `recursive` | bool | `true` | Walk GitLab subgroups. Only valid on GitLab `group` sources. |

A source can also set `retention`, `poll_interval`, `verify`, `approval` and
`prereleases`, which override [`defaults`](#defaults) for that source only.
The same rules apply.

Some asset names are always skipped: `*.src.rpm`, `*-dbgsym*` and
`*-debuginfo*`, and files without a `.deb` or `.rpm` extension (or `.rpm`
when rpm is disabled).

On its first run, a source syncs only the latest release of each repository.
Later runs pick up every release published since then.

### Asset rules

Each entry of `assets`:

| Key | Type | Default | Description |
|---|---|---|---|
| `match` | glob, **required** | none | Asset file names this rule applies to. |
| `suite` | suite name | empty | Target for `.deb` assets. Must be listed in `deb.suites`. |
| `release` | release name | empty | Target for `.rpm` assets. Must be listed in `rpm.releases`. |

Set `suite`, `release` or both. Rules are evaluated in order. The first rule
whose `match` fits the asset name and that sets a target for the asset's
format wins, so a `*` rule with only `suite` does not catch rpm assets.

- With no rules at all, every asset goes to `stable`.
- With rules, an asset that no rule places is skipped and not downloaded.
- If two assets of one release end up as the same package, version,
  architecture and distribution, both are skipped and a conflict is
  recorded.

## Changes that need a restart

A reload applies everything except these keys. Changing them makes the reload
fail with a message naming the keys, and the running configuration stays
active until you restart:

- `server.listen`
- `metrics.listen`
- `database.url`
- `storage.path`

A reload restarts only the sources whose effective settings changed,
including inherited defaults and their forge's settings. The others keep
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
