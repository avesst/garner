# Verification

`garner verify` checks that the published repository is what the database
says it should be, by reading it the way apt and dnf do. It changes
nothing. Run it whenever you want to know that clients will not fail.

## How it works

garner runs the check under the publish lock, so no publish or GC changes
the tree while it reads. It checks:

- **The core:** `public/current` points at the snapshot the database marks
  current; snapshot rows and directories match; every fixed link in
  `public/` resolves; `key.asc` and `key.gpg` hold the configured keys;
  every package a snapshot serves has its stored file and pool entry, with
  the recorded size.
- **deb**, from the signature down: `InRelease` and `Release.gpg` verify
  against the published keys; every file `Release` lists exists with its
  size and hash, also under `by-hash/`; every `Packages` stanza matches the
  pool and the package set.
- **rpm**, the same way: `repomd.xml.asc` verifies; every repodata file
  matches before and after decompression; `primary` matches the pool and
  the package set.
- **Images:** every manifest and tag file parses, and its digest, media
  type and tag agree with the database; tag lists list exactly the tags;
  every layer is in the pool with its size.
- **History:** the previous snapshot's index files are still served, so
  clients mid-update succeed.

The formats read their files with their own parsers, not with the code
that wrote them, so a bug in rendering is not repeated in the check.

### Errors and warnings

- An **error** is a state a client can fail on: a missing file, a wrong
  hash, a bad signature.
- A **warning** is a state the next publish or GC repairs: a publish
  waiting to run, a link no longer configured, or files nothing refers
  to.

`garner verify` exits 1 on errors, and with `--strict` on warnings too.

### Waiting for a change to land

After pushing a configuration change or uploading, the check should cover
the new state, not the one before it. `--wait 5m` retries while a publish
is pending. `--config-hash <prefix>` also waits until the configuration
with that sha256 is loaded and published:

```sh
garner verify --config-hash "$(sha256sum garner.yaml | cut -c1-12)"
```

## When to run it

- After upgrading garner, or patching the host it runs on.
- After a restore from backup, or a GC that deleted files.
- When a client reports a hash mismatch or a missing file.
- In CI, after a configuration change is deployed.

It needs the `publish` scope, and takes about a second for a repository
of hundreds of packages.

## Why it works this way

- **Read like a client.** A check that walks the database would miss what
  clients see. Starting from the signature and following hashes down is
  exactly the chain apt and dnf trust.
- **Warnings are not errors.** Unreferenced files are normal between a
  publish and the GC after it; they should not fail a CI job.

## Limits

- Stored files are compared by size and recorded hash, not hashed again,
  so bit rot inside a file of the right size is not found.

## See also

- [Publishing and snapshots](publishing.md)
- [Monitoring](../guides/monitoring.md)
- [Troubleshooting](../guides/troubleshooting.md)
