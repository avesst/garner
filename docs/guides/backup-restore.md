# Backup and restore

garner's state is four things. Back up all of them together:

| What | Default location | Why |
|---|---|---|
| The configuration | `/etc/garner/garner.yaml`, best kept in git | Formats, sources, rules |
| The signing keys | `signing.keys[].key_file`, or `/var/lib/garner/signing/key.asc` when `garner init` made it | Without them, hosts must trust a new key |
| The database | `/var/lib/garner/garner.db` (`database.url`) | Packages, owners, tokens, snapshots, sync progress |
| The storage directory | `/var/lib/garner` (`storage.path`) | The package files and the published repository |

Secrets that `*_file` keys point to, such as forge tokens, belong in the
backup too, or wherever you keep secrets.

## Back up

The database and the files must come from the same moment: the database
lists files that must exist, and GC deletes files the database no longer
needs. Stopping garner for the backup is the simple way to get that.

If a reverse proxy serves `public/` from disk, apt and dnf hosts keep
working while garner is stopped; only uploads, sync and image pulls wait.

```sh
sudo systemctl stop garner
sudo tar -C / -czf /backup/garner-$(date +%F).tar.gz \
    etc/garner var/lib/garner
sudo systemctl start garner
```

- `tar` keeps the hardlinks between the blob store and the pool, so the
  archive holds each package once. A copy tool must do the same: `rsync`
  needs `-H`.
- After a clean stop the database is one file; there is no `-wal` file to
  copy alongside it.
- With `database.url` or the key files elsewhere, add those paths.
- To keep the stop short, snapshot the filesystem instead (LVM, ZFS,
  btrfs) while garner is stopped, start garner, then archive the
  snapshot.

A backup taken while garner runs is not consistent: a publish or GC
during the copy leaves a database that refers to files the archive does
not have, or the other way round.

## Restore

On the same host or a new one with garner installed:

```sh
sudo systemctl stop garner
sudo tar -C / -xzf /backup/garner-2026-11-27.tar.gz
sudo chown -R garner:garner /var/lib/garner
sudo systemctl start garner
```

Then check the restored repository, with a token that has `publish`:

```sh
GARNER_TOKEN="admin:<secret>" garner verify
```

`no problems found` means the published tree matches the database. On a
new host, point DNS or the proxy at it once verify passes.

garner publishes at start only if something changed since the backup,
such as a newer garner version.

## What a restore loses

Everything since the backup: packages uploaded or synced since, tokens
created since, ownership changes, and newer snapshots. Sources pick up
releases published since then on their next poll, so synced packages come
back on their own; uploads must be repeated.

## Tested

This procedure was tested on a small instance (one package, deb only):
stopped, archived with `tar`, the state directory removed, restored,
started, and `garner verify` reported no problems. It has not been
tested on a large repository or with rpm.

## See also

- [Storage](../concepts/storage.md): what lives where
- [Upgrades](upgrades.md): take a backup before upgrading
