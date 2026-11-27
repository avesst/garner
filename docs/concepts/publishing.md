# Publishing and snapshots

A publish turns what garner has stored into the repository clients read.
garner never edits the published repository in place: each publish
writes a complete new **snapshot** and then switches to it in one step.

## How it works

### When garner publishes

Anything that changes what clients should see asks for a publish: an
upload, a release or image synced from a source, an image push, an
approval, a removal, a rollback, a reload, and `garner publish`. garner
waits 10 seconds after a request before it starts, so a CI job that
uploads twenty packages produces one publish, not twenty. A publish that
fails is retried after 30 seconds, then after twice as long each time, up
to every 10 minutes, so a full disk or a passing error does not leave
changes unpublished.

At start, garner publishes only if something that goes into a snapshot
has changed since the current one: its own version, the configuration,
the enabled formats, the signing keys or the set of packages. Restarting
garner therefore does not create snapshots, and upgrading it does.

### What a publish does

1. **Select.** garner takes every package placed in a suite, release or
   image name, and applies [retention](retention-gc.md).
2. **Render.** It writes the metadata of every format into a new snapshot
   directory: apt's `Packages` and `Release` files, dnf's repodata, image
   manifests and tag lists, and also the UI, the public keys and the
   client files. The UI is a static page, so its install commands, key
   and package lists always match the snapshot clients read.
3. **Carry over history.** It copies in the index files of recent earlier
   snapshots (see below).
4. **Sign.** It signs `InRelease` and `Release.gpg` for apt and
   `repomd.xml.asc` for dnf with the repository key.
5. **Switch.** It writes everything to disk, then makes the new snapshot
   the current one by renaming a single symlink, `public/current`.

A snapshot holds only metadata. The packages themselves sit once in a
shared pool, which every snapshot refers to, so a new snapshot takes no
extra disk space for the packages it lists.

### One switch for everything

Every path clients read, `/deb/dists/`, `/rpm/.../repodata/`, `/v2/`,
the UI, `key.asc` and the client files, leads through `public/current`.
Renaming that one link is atomic, so a client sees either the old
snapshot or the new one, never a mix of the two and never a half-written
file. apt's index files always match their `InRelease`, and the published
keys always match the signatures on the metadata, even while a key is
being rotated.

### Clients that are mid-update

apt and dnf fetch a repository in two steps: first the signed top-level
file (`InRelease`, `repomd.xml`), then the index files it lists. If a
publish happened between the two steps, the second could fail, because
the new snapshot lists different files.

garner avoids this by carrying over the index files of every snapshot
published within the last hour (`storage.index_history`), and always those
of the previous one. They are named by their hash, apt's `by-hash/` and
dnf's hashed repodata names, so the old files are still there for a
client that read the old top-level file, and the new top-level file lists
only the new ones.

### Rollback

`garner snapshot list` shows the kept snapshots, and
`garner snapshot rollback <id>` returns the repository to one of them. A
rollback does not point back at the old snapshot: it publishes a **new**
snapshot with the old one's packages and image tags, rendered and signed
again, with current history, so clients mid-update are not broken.

A rollback also changes what is placed: packages of the old snapshot
become active again and the others are removed, so the next upload does
not undo the rollback. A registry source does not move a rolled-back tag
until upstream moves that tag again.

### Pinning a host to a snapshot

Every kept snapshot is also served under its own ID, so a host can stay on
a known state while the repository moves on:

```
Types: deb
URIs: https://repo.example.com/snapshots/01KAB2C3D4E5F6G7H8J9K0M1N2/deb
Suites: stable
```

The same works for rpm under `/snapshots/<id>/rpm/`. Images have no
snapshot URLs; pull them by digest to pin them.

## Why it works this way

- **No half-published state.** An apt repository is several files that
  must agree. Editing them in place would leave a window in which they
  don't, and clients that hit it fail with hash mismatches.
- **Snapshots are never changed.** That is why history is copied into new
  snapshots and why a rollback writes a new snapshot: a snapshot that a
  host has pinned, or that a client is reading, stays as it was.
- **Plain files.** The published tree needs no garner process to read, so
  a reverse proxy can serve deb and rpm straight from disk while garner is
  restarting or stopped.

## Limits

- Changes are not visible at once: a publish starts 10 seconds after the
  last request. An image pushed with `docker push` can be pulled after the
  next publish.
- One publish runs at a time.
- Only the newest 30 snapshots are kept (`storage.snapshot_retention`);
  older ones cannot be rolled back to or pinned. A replaced snapshot stays
  readable for another 10 minutes (`storage.gc_grace`), so downloads
  already under way can finish.
- A proxy serving `public/` from disk serves apt and dnf, not `/v2/`;
  image pulls and pushes always go through garner.

## See also

- [Retention and garbage collection](retention-gc.md): what each publish
  keeps
- [Signing](signing.md): the key that signs each snapshot
- [Storage](storage.md): where snapshots and the pool live on disk
- Configuration: [`storage`](../reference/configuration.md#storage) for
  `index_history`, `snapshot_retention` and `gc_grace`
