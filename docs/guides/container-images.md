# Container images

This guide turns on garner's registry, pushes an image from CI, pulls it
on a host, and syncs an image from another registry. How the registry
works is in [container images](../concepts/formats/oci.md).

## 1. Turn on the registry

Add an `oci:` section to `/etc/garner/garner.yaml`:

```yaml
oci: {}
```

To give the registry a hostname of its own, so image references survive
a later move to another registry:

```yaml
oci:
  public_url: https://registry.example.com
```

Adding a format section needs a restart, not a reload:

```sh
garner config validate /etc/garner/garner.yaml
sudo systemctl restart garner
```

## 2. Pass `/v2/` to garner

The reverse proxy must send `/v2/` on the registry's host to garner, at
the host root; image clients look for it nowhere else.

Caddy:

```caddyfile
handle /v2/* {
	reverse_proxy 127.0.0.1:8080
}
```

nginx:

```nginx
location /v2/ {
    proxy_pass http://127.0.0.1:8080;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-Proto $scheme;
    client_max_body_size 0;           # garner limits each blob
    proxy_request_buffering off;
    proxy_buffering off;
}
```

With `oci.public_url` on another hostname, give that hostname a server
block of its own with the same rule. The
[README](../../README.md#put-it-behind-a-reverse-proxy) has the full
proxy examples.

## 3. Push from CI

Create a token with `upload`; its owner label owns the image names it
pushes first:

```sh
GARNER_TOKEN="admin:<secret>" garner token create \
    --name ci-tool --owner team-platform --scope upload
```

In the pipeline, log in with the token's name as the user and its secret
as the password:

```sh
echo "$GARNER_SECRET" | docker login registry.example.com -u ci-tool --password-stdin
docker build -t registry.example.com/team/tool:1.2.0 .
docker push registry.example.com/team/tool:1.2.0
```

Multi-platform images work as usual, for example with
`docker buildx build --platform linux/amd64,linux/arm64 --push`.

The tag can be pulled after the next publish, about 10 seconds later.

## 4. Pull on a host

No login is needed:

```sh
docker pull registry.example.com/team/tool:1.2.0
```

For production, pin the digest; it never changes, even when the tag
moves:

```sh
docker pull registry.example.com/team/tool@sha256:<digest>
```

The UI's Container images tab lists every image with a button that copies
its pull command.

## Sync images from another registry

To keep a copy of upstream images, add a registry and a source:

```yaml
registries:
  - name: dockerhub
    url: https://registry-1.docker.io
    auth_hosts: [auth.docker.io]
    username: example-bot
    token_file: /run/credentials/garner.service/dockerhub

sources:
  - name: alpine
    registry: dockerhub
    image: library/alpine
    tags: ["3.22", "3.22.*"]
    as: mirror/alpine
    min_release_age: 72h
```

The token comes in through systemd, as for forges (see
[syncing sources](sync-sources.md#1-add-the-forge)): add
`LoadCredential=dockerhub:/etc/garner/secrets/dockerhub.token` to a drop-in and
restart; without a new credential, a reload is enough.

Keep `tags` narrow: the first poll copies every matching tag, all
platforms included. Docker Hub limits anonymous pulls, so give it
credentials. See [registries](../concepts/registries.md) for how tags
move and how `min_release_age` works for images.

## Remove an image or a tag

Both need `admin`:

```sh
garner remove team/tool --version sha256:<digest>   # one image
garner remove team/tool                             # every image of the name
```

A tag alone is deleted through the registry API,
`DELETE /v2/<name>/manifests/<tag>`, with an admin token as Basic
credentials.

## When it does not work

| Symptom | Cause |
|---|---|
| `docker login` fails with `401` | Wrong token name or secret, or the token was revoked |
| `docker login` or push fails with `403` | The request did not reach garner over HTTPS through a trusted proxy; see [troubleshooting](troubleshooting.md#403-on-uploads) |
| Push fails with `denied`, naming an owner | The image name belongs to another owner |
| `http: server gave HTTP response to HTTPS client` | The registry is served over plain HTTP; use HTTPS, or list the host in the client's `insecure-registries` |
| Pull of a just-pushed tag fails with `manifest unknown` | The publish has not run yet; wait 10 seconds |
| Push hangs or fails on large layers through nginx | Missing `client_max_body_size 0` or `proxy_request_buffering off` |

## See also

- [Container images](../concepts/formats/oci.md)
- [Registries](../concepts/registries.md)
- Configuration: [`oci`](../reference/configuration.md#oci),
  [`registries`](../reference/configuration.md#registries)
