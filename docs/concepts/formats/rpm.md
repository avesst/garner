# rpm: the dnf repositories

With an `rpm:` section, garner serves signed dnf repositories, one per
release and architecture, from `.rpm` packages that are uploaded or
synced.

```yaml
rpm:
  architectures: [x86_64, aarch64]
  releases: [stable, el9, el10]
```

## How it works

### Releases and architectures

- **Releases** come from `rpm.releases`; the default is `stable` alone.
  Like deb suites, a release can be a channel or an OS release (`el9`).
- Each release has a repository per **architecture** in
  `rpm.architectures`, at `/rpm/<release>/<arch>/`.
- `noarch` packages are listed in every architecture's repository; the
  file is stored once.
- `i686` is multilib only: with `i686` and `x86_64` configured, the
  x86_64 repository also lists the i686 packages, as Fedora and EL ship
  them. There is no separate `i686` repository, and `i686` requires
  `x86_64`.

An upload goes into `--release`, or the default: `stable` if configured,
else the first one listed.

### The client file

garner writes a `.repo` file per release:

```
[garner]
name=garner
baseurl=https://repo.example.com/rpm/stable/$basearch/
enabled=1
gpgcheck=1
repo_gpgcheck=1
gpgkey=https://repo.example.com/key.asc
```

```sh
sudo curl -fsSLo /etc/yum.repos.d/garner.repo https://repo.example.com/garner.repo
```

The default release's file is at the root, the others at
`/clients/<release>/garner.repo`. All use the same repo ID, so installing
another release's file replaces the first rather than adding a second.
dnf imports the key from `gpgkey` on first use.

Both checks are on: `repo_gpgcheck` for the signed metadata and
`gpgcheck` for each package's own signature.

### Re-signed packages

dnf checks every package's signature against the keys it trusts, so
garner **re-signs every rpm** with the repository key at ingest, replacing
the vendor's signature. It keeps the original, so it can re-sign again
after a key change. See [signing](../signing.md#re-signing-rpm-packages).

### Layout

```
/rpm/<release>/<arch>/repodata/repomd.xml, repomd.xml.asc
/rpm/<release>/<arch>/repodata/<sha256>-primary.xml.zst, ...
/rpm/<release>/<arch>/pool/<prefix>/<hash>/<file>.rpm
```

- garner builds the repodata with `createrepo_c` and signs `repomd.xml`.
- Metadata files carry their hash in their name, and `repodata/` keeps
  those of recent snapshots too, so a `dnf makecache` during a publish
  succeeds.
- Every `pool/` leads to one shared rpm pool.

### Required tools

rpm support needs `createrepo_c`, `rpmsign`, `gpg`, `gpg-agent` and
`gpgconf` on `PATH`. garner's packages recommend them and its container
image ships them. With an `rpm:` section and a tool missing, garner
refuses to start and names the missing tools.

### Packages garner refuses

An rpm whose header text holds control characters, often colour codes in
a generated changelog, is refused at ingest: `createrepo_c` would fail on
it for the whole repository. See
[ingest](../ingest.md#packages-that-would-break-clients).

## Limits

- Re-signing needs the private key as a file `gpg` can import, so the key
  cannot sit in an HSM or KMS.
- Source rpms (`*.src.rpm`) and debuginfo packages are never synced.
- garner serves packages, not a mirror of Fedora or EL.

## See also

- [Signing](../signing.md)
- Configuration: [`rpm`](../../reference/configuration.md#rpm)
