# Storage

garner keeps two kinds of state: a **database** with what it knows about
packages, owners, tokens, snapshots and sources, and a **storage
directory** with the files themselves and the published repository.

## How it works

### The database

garner uses SQLite, in one file: `/var/lib/garner/garner.db` by default
(`database.url`). It must be on local disk, never on NFS or another
network filesystem: SQLite's locking and its write-ahead log assume local
disk.

At start garner takes a lock on `garner.db.lock` and refuses to start if
another process holds it, so two garner processes never share one
database. Database migrations apply automatically at start.

### The storage directory

Everything else lives under `storage.path`, `/var/lib/garner` by default:

```
/var/lib/garner/
  garner.db, garner.db.lock     the database (its location is database.url)
  signing/key.asc               the key garner init generated, if any
  blobs/sha256/ab/abcdef...     every stored file, named by its sha256
  pool/deb/...                  the packages and image layers clients
  pool/rpm/...                    download, as hardlinks into blobs/
  pool/oci/...
  snapshots/<id>/               one directory per snapshot: metadata,
                                  the UI, keys and client files
  uploads/                      image pushes in progress
  tmp/                          files being written
  public/                       the tree clients read
    current -> ../snapshots/<id>
    deb/, rpm/, v2, index.html, key.asc, ...   fixed links through current
```

- **Blobs** are the stored files. Each is written once, flushed to disk,
  and never changed. A file delivered twice, or placed in several suites,
  is stored once. An rpm has two blobs: the original and the re-signed
  copy that is served.
- **The pool** gives each blob the path clients download it from. Pool
  entries are hardlinks, so they take no extra space; where hardlinks are
  not possible, garner copies.
- **Snapshots** hold only metadata. See [publishing](publishing.md).
- **`public/`** is what is served. `public/current` is the only link that
  ever changes target; every other link in `public/` leads through it.

### Serving `public/`

garner's built-in server serves `public/`, the API and the registry. A
reverse proxy can also serve `public/` straight from disk, following its
symlinks. Then apt and dnf keep working while garner is stopped.

The proxy's user needs read access to the storage directory, which the
packaged install makes group `garner`.

### Disk space

Disk use is roughly every package and image layer that any kept snapshot
lists, rpm packages twice, plus metadata per snapshot. Snapshots are small
next to the packages. `garner_storage_bytes` reports the total. See
[retention and GC](retention-gc.md) for what decides how much is kept.

## Why it works this way

- **Content-addressed files.** Naming each file by its hash makes storing
  idempotent, lets suites and images share files, and lets
  [verification](verification.md) check a file against its name.
- **Hardlinks for the pool.** The pool and the blob store are the same
  files under two names, so the client-facing layout costs nothing.
- **Plain files for the repository.** The published tree needs nothing
  but a web server to read.

## Limits

- Only local or POSIX storage: there is no S3 or other object storage
  backend yet.
- The storage directory needs hardlinks, symlinks and an atomic
  `rename(2)`. Local filesystems have them. On NFS v4 a single garner has
  worked in small tests, but this is not yet tested properly.
- One garner per database and storage directory. Running several nodes on
  shared storage is planned, not built.

## See also

- [Backup and restore](../guides/backup-restore.md)
- [Publishing and snapshots](publishing.md)
- Configuration: [`database`](../reference/configuration.md#database),
  [`storage`](../reference/configuration.md#storage)
