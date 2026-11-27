# Upgrading garner

An upgrade is a package upgrade: the package restarts garner if it is
running, and garner applies any database changes at start.

## What to expect from a release

garner aims for upgrades that need nothing beyond installing the new
package, but does not guarantee it. Versions follow
[semantic versioning](https://semver.org/):

- **Before 1.0.0**, any release may contain changes that need action,
  patch releases included, as semver allows for `0.y.z` versions.
- **From 1.0.0 on**, such changes come only in a new major version;
  minor and patch releases do not need action.

Every release's notes list what needs action under
**Changes that may need action**, so an upgrade without that section
needs none.

## Before you upgrade

1. **Read the release notes** of every version between yours and the new
   one, on the [GitHub releases](https://github.com/avesst/garner/releases)
   page, in particular their **Changes that may need action**.
2. **Validate your configuration with the new binary.** A key that was
   renamed or removed fails validation, and an invalid file keeps garner
   from starting. The release's static binary,
   `garner_<version>_linux_amd64`, can check the file without installing:

   ```sh
   ./garner_0.3.0_linux_amd64 config validate /etc/garner/garner.yaml
   ```

3. **Take a backup.** Database changes apply once and are not undone; a
   backup is the way back. See [backup and restore](backup-restore.md).

## Upgrade

Install the new package as you installed the first one:

```sh
sudo apt install ./garner_0.3.0_amd64.deb
# or
sudo dnf install ./garner-0.3.0-1.x86_64.rpm
```

The package restarts garner when it is running. Under systemd, garner's
listening socket stays open across the restart, so requests wait for the
new process instead of failing. If a reverse proxy serves `public/` from
disk, apt and dnf hosts are not affected at all.

At start, garner applies the database migrations of the new version, then
publishes a new snapshot, since its own version is part of what a
snapshot records.

If garner syncs its own releases (see the
[README](../../README.md#keep-garner-up-to-date)), the host gets new
versions with its regular package updates instead.

## After the upgrade

```sh
systemctl status garner
garner status
garner verify
```

`garner status` shows the new version; `garner verify` (scope `publish`)
reads the published repository the way clients do. Then check one host:

```sh
sudo apt update      # or: sudo dnf makecache
```

## If the new version does not start

`journalctl -u garner` shows why. The usual causes:

- **The configuration fails validation**: fix it, then
  `sudo systemctl start garner`.
- **rpm tools are missing** (`missing tools: ...`): install them, or
  remove the `rpm:` section.

## Going back

Downgrading the package over a database the new version migrated is not
supported. To go back:

1. Stop garner.
2. Install the previous package.
3. Restore the backup taken before the upgrade.
4. Start garner and run `garner verify`.

Packages delivered between the upgrade and the restore are lost; synced
ones come back on the next poll.

## See also

- [Backup and restore](backup-restore.md)
- [Verification](../concepts/verification.md)
