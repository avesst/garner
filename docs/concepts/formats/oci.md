# Container images: the registry

With an `oci:` section, garner is a container registry at `/v2/`.
`docker`, `podman`, `skopeo`, `crane` and other OCI clients push to it and
pull from it, and garner can copy selected images from other
[registries](../registries.md).

```yaml
oci:
  public_url: https://registry.example.com   # optional
  retention: 10
```

## How it works

### Names, tags and digests

An image name is a path such as `team/tool`. Each push or sync stores a
**manifest**, or an **index** for a multi-platform image, named by its
digest (`sha256:...`), together with its layers. **Tags** such as `1.2.0`
and `latest` are labels that point at a digest and can move.

- A digest never changes, so `docker pull repo/tool@sha256:...` always
  returns the same image.
- When a tag moves, the digest it pointed at becomes untagged. It stays
  pullable until [retention](../retention-gc.md) ages it out, so
  deployments that pin digests keep working.
- Layers are stored once, however many images and names use them.
- An index is stored whole, every platform included.

Image names are [owned](../ownership.md) like package names: the first
push of `team/tool` makes the token's owner the owner of `team/tool`, the
full path. Nobody owns `team/` by pushing to it, so another owner can
still push `team/other`. Image names and package names are owned
separately: owning the package `tool` says nothing about the image
`tool`. Names with a path part `blobs`, `manifests`, `tags`,
`uploads` or `referrers` are refused, since they would collide with the
registry's own paths.

### Pushing

```sh
echo "$SECRET" | docker login registry.example.com -u ci-tool --password-stdin
docker push registry.example.com/team/tool:1.2.0
```

Log in with a token's name as the user and its secret as the password.
Pushing needs the `upload` scope. A push to a name owned by someone else
is refused with `DENIED` before any layer is sent. Each layer may be up to
`storage.max_package_size`, each manifest 4 MiB.

A pushed tag can be pulled after the next [publish](../publishing.md),
about 10 seconds later. `docker push` does not read it back, so this goes
unnoticed in CI. Layers garner already has are not sent again.

Deleting a tag (`DELETE /v2/<name>/manifests/<tag>`) or an image by digest
needs `admin`; `garner remove <name> --format oci --version <digest>`
does the same.

### Pulling

Pulls need no login. `docker pull registry.example.com/team/tool:1.2.0`
works from any host that can reach garner, over HTTPS, or over plain HTTP
for hosts that list the registry in `insecure-registries`.

### The registry's host

Image references start with a host: `oci.public_url`'s, or else the host
of `server.public_url`. Clients look for `/v2/` only at the host root, so
a path in `server.public_url` does not apply to images. Giving the
registry a hostname of its own, such as `registry.example.com`, lets you
move images to another registry later by changing DNS, without changing
any image references. garner answers `/v2/` on every hostname it
receives.

### Serving

garner serves `/v2/` itself. Manifests and tag lists come from the
current snapshot, like apt and dnf metadata, and layers from the pool. A
reverse proxy passes `/v2/` to garner; it cannot serve it from disk the
way it can serve `public/` for apt and dnf, so pulls pause while garner
restarts. Under systemd the listening socket stays open, so they wait
rather than fail.

Only published images are served. Layers of a held image, or of an image
not yet published, are never served, and a push cannot refer to them.

## Limits

- No snapshot URLs for images: `/snapshots/<id>/` covers apt and dnf.
  Pull by digest to pin.
- The OCI 1.1 referrers API is not implemented. cosign signatures stored
  under its tag scheme (`sha256-<hex>.sig`) work as ordinary tags; garner
  does not check them.
- Images cannot be uploaded as archives (`docker save`) through
  `garner upload`; push them.
- garner is not a pull-through cache: it serves only images pushed to it
  or named by a source.

## See also

- [Registries](../registries.md): syncing images from other registries
- [Container images guide](../../guides/container-images.md)
- Configuration: [`oci`](../../reference/configuration.md#oci)
