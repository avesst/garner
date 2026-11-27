# Registries

A registry is a configured OCI registry, such as ghcr.io, Docker Hub or a
GitLab container registry, that registry [sources](sources.md) copy
images from. garner is not a pull-through cache: it stores only the
images a source names, and only the tags that source selects.

```yaml
oci: {}

registries:
  - name: ghcr
    url: https://ghcr.io
  - name: dockerhub
    url: https://registry-1.docker.io
    auth_hosts: [auth.docker.io]
    username: example-bot
    token_env: DOCKERHUB_TOKEN

sources:
  - name: alpine
    registry: dockerhub
    image: library/alpine
    tags: ["3.22", "3.22.*"]
  - name: tool-image
    registry: ghcr
    image: example-org/tool
    tags: ["v*"]
    as: team/tool               # the name garner serves it under
```

## How it works

### Tags and digests

Each poll lists the image's tags, keeps those that match `tags`, and asks
the registry which digest each points at. A digest garner does not have
is downloaded whole: the manifest or index, every platform of an index,
and every layer, each checked against its digest. Layers garner already
has, from any image, are not downloaded again.

- **A tag that moves upstream moves in garner.** The digest it pointed at
  becomes untagged and stays pullable until `oci.retention` (or the
  source's `retention`) ages it out.
- **A tag deleted upstream stays** in garner.
- **The first run takes every matching tag**, so keep `tags` narrow.
- **A tag moves only when upstream moves it.** After a rollback, or after
  an admin deletes the tag in garner, it stays as it is until upstream
  points it at a new digest.
- **A digest garner has removed is not copied again**, whether retention,
  a rollback or a delete removed it.

### `min_release_age` for images

Registries record no push time, and an image's own creation date is often
fixed for reproducible builds. So for images the wait runs from when
garner **first saw** a digest under a tag. Each poll applies the newest
digest under the tag that is old enough. A tag that moves more often than
the wait still advances, lagging by about that age. A digest that
upstream withdraws before it is old enough, by moving the tag back, is
never applied.

### The local name

`as` sets the name garner serves the image under; without it, the image
keeps its path (`library/alpine`). The name is owned like a package name,
and `owns:` on a registry source reserves image names. Two registry
sources may not serve the same name.

### Credentials and hosts

Registries hand out tokens from a token endpoint, which is often another
host: `auth.docker.io` for Docker Hub, the GitLab host for a GitLab
registry. garner sends credentials only to the registry's own host and
to the hosts in `auth_hosts`, never to a host a response names. Layer
downloads that redirect to a CDN, as ghcr.io's and Docker Hub's do, are
fetched without credentials.

Without credentials garner pulls anonymously. Docker Hub limits anonymous
pulls per address, so give a Docker Hub registry credentials.
`max_concurrent`, `ca_file` and `proxy` work as they do for
[forges](forges.md#network). A `429` with a `Retry-After` of up to 2
minutes is waited out; a longer one fails the poll.

## Why it works this way

- **Whole indexes.** Copying every platform keeps the index's digest, and
  any signature over it, valid. Picking platforms would rewrite the index
  and change its digest.
- **First sight, not image date.** The only time garner can trust for a
  digest is its own.
- **Selected tags only.** A pull-through cache stores whatever anyone
  pulls; garner stores what the configuration names, so the registry's
  contents are reviewed in git like the rest of the configuration.

## Limits

- Platforms cannot be filtered: a multi-platform image is copied with all
  its platforms.
- cosign signatures are copied only as ordinary tags, if `tags` matches
  them; garner does not check them.
- Registry sync with credentials and `min_release_age` against a real
  registry is not yet verified; anonymous sync from ghcr.io, Docker Hub
  and registry.gitlab.com is.

## See also

- [Container images](formats/oci.md): how images are stored and served
- [Sources](sources.md)
- Configuration: [`registries`](../reference/configuration.md#registries),
  [registry sources](../reference/configuration.md#registry-sources)
