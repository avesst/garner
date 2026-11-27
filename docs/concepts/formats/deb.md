# deb: the apt repository

With a `deb:` section, garner serves one signed apt repository under
`/deb`, from `.deb` packages that are uploaded or synced.

```yaml
deb:
  architectures: [amd64, arm64]
  suites: [stable, noble, bookworm]
```

## How it works

### Suites, components and architectures

- **Suites** come from `deb.suites`; the default is `stable` alone. A
  suite can stand for a channel (`stable`, `testing`) or for an OS
  release (`noble`, `trixie`), when packages are built per release.
- There is one **component**, `main`.
- **Architectures** come from `deb.architectures`. `Release` lists every
  configured architecture, not only those with packages, and each has a
  `Packages` file, empty if need be. Packages with `Architecture: all` appear in
  every architecture's `Packages` file.

An upload goes into `--suite`, or the default suite: `stable` if
configured, else the first one listed. A synced asset goes where the
source's [asset rules](../../reference/configuration.md#asset-rules) put it.

### The client file

garner writes a deb822 `.sources` file per suite, with the repository's
public keys inline in `Signed-By`:

```
Types: deb
URIs: https://repo.example.com/deb
Suites: stable
Components: main
Signed-By:
 -----BEGIN PGP PUBLIC KEY BLOCK-----
 .
 mQINBGk...
 -----END PGP PUBLIC KEY BLOCK-----
```

A host installs that one file and nothing else:

```sh
sudo curl -fsSLo /etc/apt/sources.list.d/garner.sources https://repo.example.com/garner.sources
sudo apt update
```

The default suite's file is at the root (`/garner.sources`), the others
at `/clients/<suite>/garner.sources`. The UI shows the commands for each.
The file name comes from `server.client_name`.

Keys inline need apt 2.4 or later, which every supported release has. A
separate keyring file would have to be readable by apt's unprivileged
`_apt` user, which a keyring downloaded under a strict umask is not.

### Layout

```
/deb/dists/<suite>/InRelease, Release, Release.gpg
/deb/dists/<suite>/main/binary-<arch>/Packages, Packages.gz, Packages.xz
/deb/dists/<suite>/main/binary-<arch>/by-hash/SHA256/<hash>
/deb/pool/main/<prefix>/<name>/<hash>/<name>_<version>_<arch>.deb
```

- `InRelease` is signed inline and `Release.gpg` beside it; apt uses
  either.
- `Acquire-By-Hash: yes` makes apt fetch index files by their hash. The
  `by-hash/` directories also keep the index files of recent snapshots,
  so an `apt update` that runs during a publish succeeds. See
  [publishing](../publishing.md#clients-that-are-mid-update).
- In the pool path, `<prefix>` is the first letter of the name (four
  characters for `lib*`), and `<hash>` is the start of the file's sha256.
  The file name is built from the package's own fields, not the name it
  was uploaded under.

### Same version, different builds

Projects often build one version separately for each OS release. Two
suites can hold different files with the same name, version and
architecture; the hash in the pool path keeps them apart. Within one
suite, a version is unique: a different file for the same version is
refused (see [ingest](../ingest.md)).

A host should enable the suite for its own OS release. A host that
enables two suites holding different files for one version sees one
package, as apt treats them as the same.

### Multiarch

With `i386` configured, a host that ran `dpkg --add-architecture i386`
can install `Multi-Arch: same` packages for both architectures side by
side.

## Limits

- One component, `main`; more are planned.
- No `Valid-Until` in `Release`, so a host cannot tell metadata frozen
  by an attacker from metadata that is simply unchanged.
- Individual `.deb` files are not signed; apt trusts them through the
  hashes in the signed metadata, as it does for Debian's own archive.
- garner serves packages, not a mirror of Debian or Ubuntu.

## See also

- [Signing](../signing.md)
- [Ingest](../ingest.md): checks on `.deb` files
- Configuration: [`deb`](../../reference/configuration.md#deb)
