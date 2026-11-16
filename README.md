# garner

garner is an easy way to run apt and dnf repositories for your organization
or homelab. The packages you build and the releases you depend on become
installable with `apt install` and `dnf install`, and update along with the
rest of the system.

Setting up an internal Linux repository is harder than it should be. Pulp,
Artifactory and Nexus can do it, but they are built for many formats and large
deployments, and their setup and operation reflect that. Lighter tools such
as aptly, reprepro and createrepo_c cover one format each, so serving both deb
and rpm means running two tools, and syncing releases from forges is left to
scripts you write around them.

garner sits between the two: one static binary, SQLite and local disk, and
one YAML file that fits a GitOps workflow. It takes packages from two places:

- uploads, typically from CI pipelines;
- releases synced from GitHub, GitHub Enterprise Server, GitLab and
  Forgejo/Gitea, from single repositories or whole organizations and groups.

It checks them and serves them signed with one key, so clients trust the
repository and nothing else. It does not mirror distributions or build
packages from source.

## How it works

garner verifies upstream checksums, then signs the metadata of both formats
(`InRelease`, `repomd.xml`). rpm packages are also re-signed one by one,
because dnf checks each package's own signature; apt trusts a `.deb` through
the checksums in the signed metadata. Every change is published as an atomic
snapshot, so clients never see a half-written repository.

## Install

Releases on GitHub carry `.deb` and `.rpm` packages for amd64. To build them
or the binary yourself:

```sh
just build          # bin/garner
just packages       # dist/garner_<version>_amd64.deb and dist/garner-<version>-1.x86_64.rpm
```

`just packages` needs [GoReleaser](https://goreleaser.com) on `PATH`. The
packages install the binary, a systemd unit, a system user and an example
configuration at `/etc/garner/garner.yaml`. A container image is built from
the `Dockerfile`.

rpm support needs `createrepo_c`, `rpmsign`, `gpg` and `gpgconf` on `PATH`.
Without them garner serves deb only and logs which tools are missing.

## First start

Set `server.public_url` in `/etc/garner/garner.yaml` to the URL clients use,
then:

```sh
garner config validate /etc/garner/garner.yaml
sudo -u garner garner init
systemctl enable --now garner
```

`init` creates storage, the database, a signing key (when none is configured)
and an admin token. The token is printed once.

## Reverse proxy

garner speaks plain HTTP. Put it behind a reverse proxy such as nginx or Caddy
that terminates TLS, and set `public_url` to the proxy's URL.

Uploads and other authenticated requests are accepted only from loopback, or
from a proxy listed in `server.trusted_proxies` that sends
`X-Forwarded-Proto: https`. Other requests get `403`. A proxy on the same
host connects over loopback and needs no entry, but it must set
`X-Forwarded-Proto` (in nginx, `proxy_set_header X-Forwarded-Proto $scheme;`;
Caddy sets it by default). Without the header, garner takes its requests for
a local client and accepts them even when they arrived over plain HTTP.

The proxy can also serve `/var/lib/garner/public` straight from disk (it must
follow symlinks). apt and dnf then keep working while garner restarts or is
stopped; only uploads and sync pause.

## Use

Upload from CI:

```sh
export GARNER_URL=https://pkg.example.com
export GARNER_TOKEN=ci:...
garner upload --suite stable dist/*.deb
```

Point clients at the server: the landing page at `public_url` offers the
signing key and an apt or dnf client file per suite.

Reload the configuration with `garner reload` or `SIGHUP`. `garner help`
lists every command.

## Documentation

- [docs/configuration.md](docs/configuration.md): every configuration key.
- [docs/design.md](docs/design.md): architecture and the decisions behind it.
- `garner(1)` and `garner.yaml(5)`: man pages, in `docs/` and in the package.

## Development

```sh
just check          # gofmt, go vet and unit tests, as CI runs them
just e2e            # real apt and dnf clients in containers; needs Docker
just test-packages  # install dist/ packages in containers; needs Docker
```

Versions are [semver](https://semver.org). A release is a tag such as `v1.2.3`
or `v1.3.0-rc.1`: CI builds and tests the packages and attaches them, with
`checksums.txt`, to a draft GitHub release, which is published by hand. Builds
between tags get the next patch version as a pre-release, for example
`1.2.4-dev.<commit time>+<hash>` from `just packages` (`1.2.4~dev...` in package
versions, so it sorts before 1.2.4), and Go's pseudo-version from `just build`.
