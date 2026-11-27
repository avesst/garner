# Uploading packages from CI

This guide sets up a CI pipeline that uploads its `.deb` or `.rpm` files
to garner on every release. For container images, see
[container images](container-images.md).

garner must be behind a reverse proxy with HTTPS: tokens are accepted
from other hosts only over HTTPS through a proxy in
`server.trusted_proxies` (see [security](../concepts/security.md)).

## 1. Create a token for the pipeline

Give each pipeline, or each team, its own token with the `upload` scope
only. The owner label decides who owns the package names it uploads:

```sh
export GARNER_URL=https://pkg.example.com
GARNER_TOKEN="admin:<secret>" garner token create \
    --name ci-tool --owner team-platform --scopes upload
```

It prints `GARNER_TOKEN=ci-tool:<secret>`, once. Store that value as a
secret in the CI system, for example as `GARNER_TOKEN`.

Tokens of the same team should share the owner label: they can then all
upload the team's packages, and one can be revoked without the others
losing anything.

## 2. Upload from the pipeline

Two ways work equally: the `garner` CLI, or plain `curl` against the API.

**With the CLI.** Every garner release carries a static binary,
`garner_<version>_linux_amd64`, for CI images without a package manager:

```sh
garner upload --suite stable dist/*.deb
garner upload --release el9 dist/*.el9.*.rpm
```

`GARNER_URL` and `GARNER_TOKEN` come from the environment. With both
`--suite` and `--release`, one command can upload `.deb` and `.rpm` files
together; each goes to its own format's target.

The CLI sends each file in its own request, in the order given, and
carries on after a failure. It prints one line per file and exits 1 if
any file failed. See [when some files fail](#when-some-files-fail).

**With curl,** one request per file:

```sh
curl -fsS -u "$GARNER_TOKEN" -T dist/tool_1.4.0_amd64.deb \
    "$GARNER_URL/api/v1/upload?suite=stable"
```

`-u` takes the `name:secret` form of the token as it is.

Without `--suite` or `--release`, a package goes into `stable`, or the
first configured suite or release when there is no `stable`. To publish one
package into several suites, upload it once per suite.

### GitHub Actions

```yaml
- name: Upload to garner
  if: startsWith(github.ref, 'refs/tags/v')
  env:
    GARNER_URL: https://pkg.example.com
    GARNER_TOKEN: ${{ secrets.GARNER_TOKEN }}
  run: |
    for f in dist/*.deb; do
      curl -fsS -u "$GARNER_TOKEN" -T "$f" "$GARNER_URL/api/v1/upload?suite=stable"
    done
```

The runner must reach garner, so this usually means a self-hosted runner
inside your network.

### GitLab CI

```yaml
upload:
  stage: deploy
  rules:
    - if: $CI_COMMIT_TAG
  script:
    - for f in dist/*.deb; do
        curl -fsS -u "$GARNER_TOKEN" -T "$f" "$GARNER_URL/api/v1/upload?suite=stable";
      done
```

Set `GARNER_URL`, and `GARNER_TOKEN` as a masked variable, in the
project's or group's CI/CD settings.

### Forgejo Actions

The GitHub Actions step works unchanged, with the secret set in the
repository's or organization's Actions settings.

## 3. Check the result

The upload answers with what garner made of the file:

```
tool_1.4.0_amd64.deb: uploaded as tool 1.4.0 amd64 in stable (active)
```

garner publishes about 10 seconds later. To make the pipeline wait until
the package is live and the repository consistent, end it with a token
that also has `publish`:

```sh
garner verify --wait 5m
```

## What the answers mean

| Status | Meaning | What to do |
|---|---|---|
| `200` | Stored, or the same file was already there | Nothing |
| `400` | Not a valid package, or a suite, release or architecture that is not configured; the message lists the configured ones | Fix the file or the target, or add it to the configuration |
| `401` | No credentials, or an unknown or revoked token | Check the token's name and secret |
| `403` | The token lacks `upload`, or the request did not arrive over HTTPS through a trusted proxy | See [troubleshooting](troubleshooting.md#403-on-uploads) |
| `409` | The name belongs to another owner, or this version is already in the suite with different content | Ask the owner, or release a new version: a version cannot be replaced |
| `413` | Larger than `storage.max_package_size` | Raise the limit, and the proxy's body limit with it |
| `500` | An error inside garner, such as a full disk or a failed rpm signing | The garner log has the cause; retry once it is fixed |

## When some files fail

An upload is per file, not per release. Each file garner accepts is
stored and published on its own, whether or not the other files of the
same command or pipeline were accepted. A failed run can therefore leave
a release partly published: the amd64 package of version 1.4.0 live, for
example, and the arm64 one refused for an unconfigured architecture.
Hosts then see 1.4.0 on one architecture and not the other.

Accepted files are usually published together, since garner waits 10
seconds after the last upload before it publishes, but that is not a
guarantee: uploads more than 10 seconds apart can land in separate
snapshots.

To recover, fix the cause and run the same upload again. Retrying is
safe: a file garner already has is accepted again without change and
reported as `already present`, so only the files that failed are new.
To take a partial release back instead, remove it with
`garner remove <name> --version <version>` (scope `admin`).

Order the uploads so that a failure does least harm: if hosts install
packages that depend on each other, upload the dependencies first.

## Things to know

- **Versions are immutable.** Rebuilding the same version gives a
  different file, which garner refuses for a suite that already has that
  version. Bump the version, even for a rebuild.
- **The first upload claims the name.** If a sync source should own a
  name, reserve it with `owns:` before any pipeline uploads it. See
  [ownership](../concepts/ownership.md).
- **Uploads are never held** for approval, and never wait for
  `min_release_age`.
- **Retention applies.** garner keeps the newest 3 versions per name,
  suite and architecture by default (`defaults.retention`).

## See also

- [Ingest](../concepts/ingest.md): the checks every upload goes through
- [Reference: HTTP API](../reference/api.md)
