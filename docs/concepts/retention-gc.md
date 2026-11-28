# Retention and garbage collection

garner keeps a limited number of versions of each package, so a repository
that receives a release a day does not grow forever. **Retention** decides
which packages the next snapshot lists. **Garbage collection** (GC) later
deletes the files that no kept snapshot needs any more.

## How it works

### Retention

At every [publish](publishing.md), garner keeps:

- **Packages:** the newest 3 versions of each package name, per suite or
  release and per architecture (`defaults.retention`). Versions are
  compared the way dpkg and rpm compare them, not by upload time. A source
  can set its own `retention`; it applies to the packages that source
  delivers, and `defaults.retention` applies to uploads.
- **Images:** every tagged image, plus the newest 10 untagged ones per
  image name (`oci.retention`). An image becomes untagged when its tag
  moves to a newer push or sync, so a deployment that pinned the old
  digest keeps working until it ages out. A registry source can set its
  own `retention` for its images; `defaults.retention` does not apply to
  images.

A package that falls out of retention leaves future snapshots. It is not
deleted at once: the snapshots that still list it keep it reachable, and a
[rollback](publishing.md#rollback) to one of them brings it back.

`garner remove <name>` takes a package out of future snapshots by hand,
optionally only one `--version`, `--arch`, `--distribution` or
`--format`. For an image, `--version` is the digest. Without `--format`, a
package and an image of the same name are both removed. It needs the
`admin` scope.

### Garbage collection

GC runs after every publish and once an hour, and `garner gc` (scope
`admin`) runs it at once. It deletes, in this order of reasoning:

1. **Snapshots** beyond the newest 30 (`storage.snapshot_retention`). The
   current snapshot is never deleted, and a snapshot that was replaced is
   kept for 10 minutes more (`storage.gc_grace`) so requests already
   reading it can finish.
2. **Package files** that no kept snapshot lists: the stored file, its
   re-signed rpm variant, and its entry in the pool.
3. **Image uploads** that never became part of an image: blobs pushed or
   synced that no manifest uses yet, and idle upload sessions, once they
   are older than 24 hours (`oci.upload_expiry`).
4. **Leftovers** of a publish that crashed before it finished, once older
   than `storage.gc_grace`.

A file is deleted only when nothing that is kept refers to it. Layers
shared by several images, or one `.deb` placed in two suites, stay as long
as one of them is kept.

With the storage directory on NFS, a deleted file that a client is still
downloading stays as a `.nfs…` file until the download ends. GC leaves it
and removes it in a later run; `in_use` in its result and log counts such
files.

## Why it works this way

- **Retention by version, not by age.** A package that rarely changes
  keeps its last versions however old they are; a package released daily
  keeps a few days. Hosts can always downgrade by a version or two.
- **Two steps.** Retention changes only what the next snapshot lists, so
  it is undone by a rollback. Only GC deletes, and only what no kept
  snapshot refers to, so rollback and snapshot pinning stay safe for every
  kept snapshot.
- **Grace periods.** A client that read a snapshot a moment before it was
  replaced may still be downloading from it. `gc_grace` and
  `upload_expiry` keep files until such requests and pushes in progress
  are done.

## Limits

- Retention counts per name, suite or release, and architecture. There is
  no total size limit; watch `garner_storage_bytes`.
- A package outside retention can be restored only by rolling back to a
  snapshot that still lists it; once GC has deleted it, it must be
  delivered again.
- Keeping more snapshots keeps more package files: every package any kept
  snapshot lists stays on disk.

## See also

- [Publishing and snapshots](publishing.md)
- [Storage](storage.md): what takes up space on disk
- Configuration: [`defaults.retention`](../reference/configuration.md#defaults),
  [`oci.retention` and `oci.upload_expiry`](../reference/configuration.md#oci),
  [`storage`](../reference/configuration.md#storage)
