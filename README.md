# garner

garner sets up repositories for packages and container images for your
organization or homelab, and fills them with the releases published on your
forges and registries, by your own projects and the ones you depend on.

**Repositories that are easy to set up.** garner is one static binary, using
SQLite and local disk, configured by one YAML file that fits a GitOps
workflow. `garner init` creates the database and a signing key. The landing
page offers the public key and ready-made apt and dnf client files, so adding
the repository to a machine is a matter of downloading them.

**Builds synced from where they are released.** garner watches GitHub, GitHub
Enterprise Server, GitLab and Forgejo/Gitea for new releases, from single
repositories or whole organizations and groups, and publishes their `.deb`
and `.rpm` assets on its own. Packages can also be uploaded directly,
typically from CI pipelines.

**Container images too.** With the `oci` section, garner is also a
registry: CI pushes with `docker push`, hosts pull with `docker pull`, and
images can be synced from ghcr.io, Docker Hub or a GitLab registry like
releases are from forges.

garner serves apt and dnf repositories and container images. It does not
mirror distributions or build packages from source.

> [!WARNING]
> garner is under active development and is not ready for internet-facing
> use. Run it on an internal network, with access limited to the hosts and
> networks that need it.

## Where packages come from

Packages reach the repository in one of two ways. Both end up in the same
repository, and both follow the same name ownership and retention.

### Uploads

You publish a package yourself, typically as the last step of a CI
pipeline, with a token that has the `upload` scope. Create one with the
admin token that `garner init` prints:

```sh
export GARNER_URL=https://pkg.example.com
GARNER_TOKEN="admin:<secret>" garner token create --url "$GARNER_URL" \
    --name ci-mytool --owner team-a --scope upload
```

Then, in the pipeline:

```sh
export GARNER_URL=https://pkg.example.com
export GARNER_TOKEN="ci-mytool:<secret>"
garner upload --suite stable dist/mytool_1.2.0_amd64.deb
```

For CI images without a package manager, each release also carries
`garner` as a static binary, `garner_<version>_linux_amd64`.

The first upload of a package name makes the token's owner (`team-a`) the
owner of that name. Uploads of the name under any other owner are refused,
so one team cannot replace another team's package.

### Synced releases

You list sources in the configuration: a single repository, a GitHub or
Forgejo organization, or a GitLab group. garner polls each source (hourly by
default), downloads the `.deb` and `.rpm` assets of new releases, and
publishes them. Nobody has to upload anything.

```yaml
# Suites and releases that asset rules can target.
deb:
  architectures: [amd64, arm64]
  suites: [stable, testing]

rpm:
  architectures: [x86_64]
  releases: [stable, testing]

forges:
  - name: github
    type: github                # github.com; set url for Enterprise Server
    token_env: GITHUB_TOKEN     # recommended; see the rate limits below
  - name: gitlab
    type: gitlab
    url: https://gitlab.example.com
    token_file: /etc/garner/gitlab-token

sources:
  # One repository. Only this source may deliver the names in owns.
  # Assets with -beta in their file name go to testing, the rest to
  # stable.
  - name: mytool
    forge: github
    repo: example-org/mytool
    owns: ["mytool", "mytool-*"]
    prereleases: true           # also sync releases marked pre-release
    verify: required            # reject assets without a checksum
    checksums: ["SHA256SUMS"]
    min_release_age: 72h        # publish a release once it is 3 days old
    assets:
      - match: "*-beta*"
        suite: testing
        release: testing
      - match: "*"
        suite: stable
        release: stable

  # Every project in a GitLab group and its subgroups, polled every
  # 15 minutes. New packages wait for garner approve.
  - name: platform
    forge: gitlab
    group: platform
    include: ["svc-*", "lib-*"]   # project names to sync
    exclude: ["svc-sandbox-*"]    # skipped even if include matches
    poll_interval: 15m
    approval: manual
    retention: 5                # versions kept per package
```

Each asset is checked against the forge's digest or the release's checksum
file when one exists, and rejected on a mismatch. Asset rules decide which
suite or release an asset goes to. A source can reserve package names
(`owns`) so that only it may deliver them, and with `approval: manual` new
packages wait for `garner approve`. With `min_release_age`, a release is
published only once it is that old, so a compromised release has time to
be noticed and pulled first; `garner sync <source> --ignore-age <tag>`
lets an urgent fix through. `garner sync <source>` polls a source right
away.

Every poll is a few API requests, and forges limit how many they accept.
Give each forge a token: it is required for private repositories, and on
GitHub it lifts the limit from 60 requests an hour per IP address to 5,000
an hour for the token's user. 60 covers a few repositories, not an
organization. A token with read-only access to the repositories is enough,
and garner's requests for releases that have not changed do not count
against GitHub's limit when they carry a token. The metric
`garner_forge_ratelimit_remaining` shows how much quota is left.

### Container images

With an `oci` section, garner is a registry at `/v2/` on its host. Push
with a token that has the `upload` scope, its name as the user and its
secret as the password:

```sh
echo "$SECRET" | docker login pkg.example.com -u ci-mytool --password-stdin
docker push pkg.example.com/team/mytool:1.2.0
```

Pulls need no login. A pushed tag can be pulled after the next publish, a
few seconds later. Names are owned like package names, so the first push
of `team/mytool` makes the token's owner its owner. Images can also be
synced from other registries:

```yaml
oci: {}

registries:
  - name: ghcr
    url: https://ghcr.io
    username: example-bot
    token_env: GHCR_TOKEN       # a token with read:packages
  - name: dockerhub
    url: https://registry-1.docker.io
    auth_hosts: [auth.docker.io]

sources:
  - name: mytool-image
    registry: ghcr
    image: example-org/mytool
    tags: ["v*"]
    as: team/mytool             # the name garner serves it under
  - name: alpine
    registry: dockerhub
    image: library/alpine
    tags: ["3.22", "3.22.*"]
    min_release_age: 72h
```

Tags move when they move upstream, and the digest they pointed at stays
pullable until `oci.retention` ages it out, so deployments that pin
digests keep working. garner does not sign images; cosign signatures
pushed as tags (`sha256-<digest>.sig`) are stored and served like any
other tag.

## Signing and publishing

garner signs everything with one key, so clients trust the repository and
nothing else. It signs the metadata of both formats (`InRelease`,
`repomd.xml`) and re-signs rpm packages one by one, because dnf checks each
package's own signature; apt trusts a `.deb` through the checksums in the
signed metadata. Every change is published as an atomic snapshot, so clients
never see a half-written repository. `garner snapshot rollback <id>`
republishes an earlier snapshot's packages, and clients can pin to a snapshot
under `/snapshots/<id>/`.

## Install

Install the package, configure and start the service, and put it behind a
reverse proxy. Optionally, let garner keep itself up to date.

garner runs on Linux amd64 with systemd. Its packages are tested on
Debian 13, Ubuntu 26.04 LTS, AlmaLinux 10 and Fedora 44. apt and dnf as
clients are tested on Debian 12 and 13, Ubuntu 22.04, 24.04 and 26.04 LTS,
AlmaLinux 9 and 10, Rocky Linux 10 and Fedora 44.

### Install the package

Each [GitHub release](https://github.com/avesst/garner/releases) carries a
`.deb`, an `.rpm` and a static binary for Linux on amd64, with `SHA256SUMS`.

Find the latest release (or replace the first two lines with
`VERSION=0.1.0`):

```sh
url=$(curl -fsSL -o /dev/null -w '%{url_effective}' https://github.com/avesst/garner/releases/latest)
VERSION=${url##*/v}
base=https://github.com/avesst/garner/releases/download/v$VERSION
```

Debian and Ubuntu:

```sh
curl -fL -O "$base/garner_${VERSION}_amd64.deb" -O "$base/SHA256SUMS"
sha256sum --ignore-missing -c SHA256SUMS
sudo apt install "./garner_${VERSION}_amd64.deb"
```

Fedora, RHEL and derivatives:

```sh
curl -fL -O "$base/garner-${VERSION}-1.x86_64.rpm" -O "$base/SHA256SUMS"
sha256sum --ignore-missing -c SHA256SUMS
sudo dnf install "./garner-${VERSION}-1.x86_64.rpm"
```

The packages install the binary, a systemd unit, a system user and an example
configuration at `/etc/garner/garner.yaml`.

rpm support needs `createrepo_c`, `rpmsign`, `gpg` and `gpgconf` on `PATH`.
The packages recommend them, so apt and dnf install them by default. If you
install without recommended packages (`--no-install-recommends`, or dnf with
`install_weak_deps=False`), install the tools yourself or remove the `rpm`
section from the configuration; otherwise garner refuses to start.

### Configure and start

The package does not enable the service, because garner needs a
configuration and a signing key first. In `/etc/garner/garner.yaml`, set
`server.public_url` to the URL clients use. The `deb` and `rpm` sections
each enable a format; remove the one you don't need. With `rpm` present and
its tools missing, garner refuses to start. Then:

```sh
garner config validate /etc/garner/garner.yaml
sudo -u garner garner init
sudo systemctl enable --now garner
```

`init` creates storage, the database, a signing key (when none is configured)
and an admin token. The token is printed once.

Client setup instructions for apt and dnf, and for pulling and pushing
images, are on the landing page at `public_url`, with a tab per ecosystem
that also lists every package or image, filterable and a page at a time.

### Put it behind a reverse proxy

garner speaks plain HTTP. Put it behind a reverse proxy such as nginx or Caddy
that terminates TLS, and set `public_url` to the proxy's URL. With the proxy
on the same host, set `server.listen` to `127.0.0.1:8080` so that garner is
reachable only through it.

HTTPS is recommended because clients fetch the signing key from the
repository itself, and TLS is what protects that first download. Plain HTTP
is fine for reads when clients get the key another way, for example from
configuration management. Uploads from other hosts always need HTTPS.

Uploads and other authenticated requests are accepted only from loopback, or
from a proxy listed in `server.trusted_proxies` that sends
`X-Forwarded-Proto: https`. Other requests get `403`. A proxy on the same
host connects over loopback and needs no entry.

A same-host proxy must set `X-Forwarded-Proto` (in nginx,
`proxy_set_header X-Forwarded-Proto $scheme;`; Caddy sets it by default).
Without it, garner can't tell proxied requests from local ones and accepts
tokens that arrived over plain HTTP.

The proxy can also serve `/var/lib/garner/public` straight from disk (it must
follow symlinks). apt and dnf then keep working while garner restarts or is
stopped; only uploads and sync pause. The examples below do that, and pass
only the API to garner. `/healthz`, `/readyz` and `/metrics` are therefore
not reachable through the proxy; monitor them on garner's own address, or
serve `/metrics` on a separate one with `metrics.listen`.

The proxy's user needs read access to the state directory, and the proxy
must be restarted to pick up the new group:

```sh
sudo usermod -aG garner caddy      # or www-data for nginx on Debian
sudo systemctl restart caddy
```

With the `oci` section, also pass `/v2/` to garner: image pushes and the
registry's headers need it. Image pulls then pause while garner restarts;
under systemd the listening socket stays open, so they wait rather than
fail.

Caddy:

```caddyfile
pkg.example.com {
	handle /api/* {
		reverse_proxy 127.0.0.1:8080
	}
	handle /v2/* {
		reverse_proxy 127.0.0.1:8080
	}
	handle {
		root * /var/lib/garner/public
		file_server
	}
}
```

nginx:

```nginx
server {
    listen 443 ssl;
    server_name pkg.example.com;
    ssl_certificate     /etc/ssl/certs/pkg.example.com.pem;
    ssl_certificate_key /etc/ssl/private/pkg.example.com.key;

    root /var/lib/garner/public;

    location /api/ {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header X-Forwarded-Proto $scheme;
        client_max_body_size 2g;          # storage.max_package_size
        proxy_request_buffering off;
    }

    location /v2/ {                       # with the oci section
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-Proto $scheme;
        client_max_body_size 0;           # garner limits each blob
        proxy_request_buffering off;
        proxy_buffering off;
    }
}
```

### Keep garner up to date

garner can sync its own releases, which also makes a good first source. Add
this to `/etc/garner/garner.yaml`:

```yaml
forges:
  - name: github              # skip if you already have a github forge
    type: github

sources:
  - name: garner
    forge: github
    repo: avesst/garner
    owns: ["garner"]
```

Then reload with `sudo systemctl reload garner`, and add the repository on
the garner host itself as the landing page describes. From then on, new
garner releases arrive with the host's regular package updates.

If a release breaks garner, the host can't fetch the fix from its own
repository. Install the fixed package from GitHub as described above.

## Documentation

See the man pages `garner(1)` and `garner.yaml(5)`, installed with the
package, and the [`docs/`](docs/) directory.
