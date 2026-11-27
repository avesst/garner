# Getting started

This tutorial takes you from nothing to a working apt repository: install
garner, start it, upload a package you build on the spot, and install that
package with apt. It takes one Debian or Ubuntu host, and a second host
for the client if you have one. Everything runs over plain HTTP on one
port; [Next steps](#next-steps) covers HTTPS, sync and images.

## 1. Install garner

Download the latest `.deb` from the GitHub releases and check it:

```sh
url=$(curl -fsSL -o /dev/null -w '%{url_effective}' https://github.com/avesst/garner/releases/latest)
VERSION=${url##*/v}
base=https://github.com/avesst/garner/releases/download/v$VERSION
curl -fL -O "$base/garner_${VERSION}_amd64.deb" -O "$base/SHA256SUMS"
sha256sum --ignore-missing -c SHA256SUMS
sudo apt install "./garner_${VERSION}_amd64.deb"
```

The package installs the binary, a systemd unit, a `garner` system user and
a starting configuration at `/etc/garner/garner.yaml`. It does not start
the service yet.

## 2. Configure it

Open `/etc/garner/garner.yaml`. For this tutorial, change two things:

- set `server.public_url` to the address other hosts reach this one on,
  for example `http://garner.example.lan:8080`;
- delete the `rpm:` section, so only the apt repository is served.

Then check the file:

```sh
garner config validate /etc/garner/garner.yaml
```

It prints `valid` and the file's hash, or every problem with its key.

## 3. Initialize and start

```sh
sudo -u garner garner init
```

`init` creates the database, the storage directory and a signing key, and
prints an admin token, once:

```
admin token (shown once; scopes upload,publish,admin):
  GARNER_TOKEN=admin:garner_zfIiwK...
```

Keep it: you need it in step 5. Then start garner:

```sh
sudo systemctl enable --now garner
curl -fsS http://localhost:8080/readyz
```

`ready` means garner has published its first, empty snapshot. Open
`public_url` in a browser: the page shows the repository, still without
packages, and how to add it to a host.

## 4. Build a package

Any `.deb` works. To have one at hand, build an empty package:

```sh
mkdir -p hello/DEBIAN
cat > hello/DEBIAN/control <<'EOF'
Package: hello-garner
Version: 1.0.0
Architecture: all
Maintainer: You <you@example.com>
Description: test package for garner
EOF
dpkg-deb --root-owner-group --build hello hello-garner_1.0.0_all.deb
```

## 5. Upload it

Create a token for uploads, so the admin token stays out of everyday use.
On the garner host, the API is reachable over loopback without HTTPS:

```sh
export GARNER_TOKEN="admin:<secret from step 3>"
garner token create --url http://localhost:8080 --name tutorial --owner me --scope upload
```

It prints the new token as a `GARNER_TOKEN=tutorial:...` line. Use it to
upload:

```sh
export GARNER_TOKEN="tutorial:<secret>"
garner upload --url http://localhost:8080 hello-garner_1.0.0_all.deb
```

```
hello-garner_1.0.0_all.deb: uploaded as hello-garner 1.0.0 all in stable (active)
```

The package goes into `stable`, and `me` now owns the name
`hello-garner`. About ten seconds later garner publishes a new snapshot
with it; the UI lists it after a reload of the page.

## 6. Install it with apt

On any Debian 12 or newer, or Ubuntu 22.04 or newer, host that can reach
garner and has `curl`, install the client file the UI shows. It carries
the repository's key, so there is nothing else to set up:

```sh
sudo curl -fsSLo /etc/apt/sources.list.d/garner.sources http://garner.example.lan:8080/garner.sources
sudo apt update
sudo apt install hello-garner
```

apt checked the signature on the repository's metadata with garner's key,
and the package against the hash in it.

## 7. Check the repository

```sh
GARNER_TOKEN="admin:<secret>" garner verify --url http://localhost:8080
```

```
snapshot 01M8EZQW1C6M09NYFFBPGS0AC4 (configuration fc723bb75ef9), 1 packages
no problems found
```

`verify` reads the published repository as apt does and compares it with
garner's database.

## What happened

- The upload went through [ingest](concepts/ingest.md): garner read the
  package's name and version from the file, checked that apt could read
  it, gave `me` the [name](concepts/ownership.md), and stored it.
- garner then [published](concepts/publishing.md) a new snapshot with the
  package and switched to it in one step, signed with the key `init`
  made ([signing](concepts/signing.md)).

## Next steps

- **HTTPS and uploads from other hosts.** Put garner behind a reverse
  proxy; the [README](../README.md#put-it-behind-a-reverse-proxy) has
  Caddy and nginx examples. Tokens are accepted from other hosts only
  over HTTPS.
- **Packages from CI.** See [CI uploads](guides/ci-uploads.md).
- **Releases synced from GitHub, GitLab or Forgejo.** See
  [syncing sources](guides/sync-sources.md).
- **dnf and container images.** Add the `rpm:` or `oci:` section back;
  see [rpm](concepts/formats/rpm.md) and the
  [container images guide](guides/container-images.md).
- **Before relying on it:** set up [backups](guides/backup-restore.md)
  and [monitoring](guides/monitoring.md).
