# Troubleshooting

Start with these three; between them they explain most problems:

```sh
garner status            # sources, conflicts, held packages, the current snapshot
garner verify            # whether clients can fail on the published tree (scope publish)
journalctl -u garner     # garner's own log
```

## garner does not start

| Log line | Cause | Fix |
|---|---|---|
| `database is in use by another garner process` | Another `garner serve`, or a local `garner token` command, holds the database lock | Stop the other process |
| `missing tools: ...` | The `rpm:` section is configured, but `createrepo_c`, `rpmsign`, `gpg`, `gpg-agent` or `gpgconf` is not installed | Install them, or remove the `rpm:` section |
| A key path and a validation message | The configuration file is invalid | `garner config validate` lists every problem; fix them |

## A reload is refused

garner refuses a reload for one of two reasons, and keeps running on the
last good configuration either way.

### The change needs a restart

The log names the keys that changed:

```
changing server.listen needs a restart
```

These apply only at start:

- `server.listen`
- `metrics.listen`
- `database.url`
- `storage.path`
- adding or removing a format section (`deb`, `rpm`, `oci`)

Restart garner to apply them:

```sh
sudo systemctl restart garner
```

### The file is invalid

The log names each key and what is wrong with it;
`garner config validate` shows the same list. Fix the file and reload
again.

## 403 on uploads

`credentials are only accepted over HTTPS through a trusted proxy, or
from loopback`: the request did not come the way garner accepts tokens.
Check, in order:

1. The client uses an `https://` URL.
2. The proxy's address is in `server.trusted_proxies`. A proxy on the same
   host connects over loopback and needs no entry.
3. The proxy sets `X-Forwarded-Proto` to `https`. Caddy does by default;
   nginx needs `proxy_set_header X-Forwarded-Proto $scheme;`.

`token "..." lacks the ... scope`: the token is fine but has the wrong
scopes. Scopes do not imply each other; an `admin` token cannot upload
without `upload`.

## 409 on uploads

- `name "tool" is owned by team-a` or `is reserved for source:tool`: the
  name belongs to someone else. See [ownership](../concepts/ownership.md).
  An admin can move it with `garner owner set`.
- `... is already in stable with different content`: this version is
  already published with another file. Versions cannot be replaced;
  release a new version.

## Held packages

`garner status` shows held packages, and `garner packages list` shows them
with state `held`. A package is held because:

- a sync delivered a name owned by someone else (`garner conflicts list`
  shows which), or
- its source has `approval: manual`.

`garner approve <id>` publishes it; for a name owned by someone else that
needs `publish` and `admin`. `garner remove <name> --version <v>` drops it
instead.

## A source delivers nothing

`garner status` shows the last error per source. `garner status --json`
adds `repos_without_assets` (no asset matched the rules) and
`skipped_assets` (architecture, suite or release not configured), and
lists `deferred_releases` waiting for `min_release_age`.

- **Rate limited:** errors mention the rate limit, and
  `garner_forge_ratelimit_remaining` is low. Give the forge a token.
- **Private repository, downloads fail:** the forge's `url` must be the
  address the forge puts in its asset links; links to other hosts are
  fetched without the token.
- **A checksum conflict** (`garner conflicts list --all`): the asset did
  not match the forge's digest or the release's checksum file, and was
  rejected. Find out why before you work around it.
- **The source only took one release:** a new source starts from the
  latest release; older ones are not imported.

See [syncing sources](sync-sources.md#when-nothing-arrives).

## apt or dnf fail

| Error | Cause |
|---|---|
| `NO_PUBKEY` or `The following signatures couldn't be verified` (apt) | The host's `.sources` file lacks the current key, typically after a key rotation; download it again |
| `GPG check FAILED` or `repomd.xml GPG signature verification error` (dnf) | Same, for dnf; `sudo rpm --import <public_url>/key.asc` |
| `Hash Sum mismatch` or `404` on an index file | Should not happen, since garner keeps recent index files; run `garner verify` and report what it finds |
| A package is missing that was uploaded | Check `garner packages list --name <name>`: it may be held, outside retention, or in another suite or release |

`garner verify` reads the tree exactly as clients do, from the signature
down, so it pins most client errors to a file.

## Images

See the table in the
[container images guide](container-images.md#when-it-does-not-work).

## Disk fills up

`garner_storage_bytes` shows the size. Every package and layer that any
kept snapshot lists stays on disk, so space is freed by keeping fewer
versions (`defaults.retention`, `oci.retention`) or fewer snapshots
(`storage.snapshot_retention`). GC runs after every publish and hourly;
`garner gc` runs it at once.

## See also

- [Monitoring](monitoring.md)
- [Verification](../concepts/verification.md)
