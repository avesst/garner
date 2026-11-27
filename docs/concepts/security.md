# Security and tokens

garner's model is simple: **reading is open, writing needs a token.**
Anyone who can reach garner can install packages, pull images and see what
the repository holds. Every change, from an upload to a reload, needs a
token with the right scope.

## How it works

### Tokens and scopes

A token has a name, an owner label and one or more scopes:

| Scope | Allows |
|---|---|
| `upload` | Uploading packages and pushing images, under names the token's owner owns or claims |
| `publish` | Triggering a publish or a sync, approving held packages, rolling back, running `garner verify`, listing conflicts |
| `admin` | Removing packages and images, changing owners, managing tokens, reloading, running GC |

Scopes do not imply one another: an `admin` token cannot upload unless it
also has `upload`. Give each token only the scopes its job needs: a CI
pipeline usually needs `upload` alone.

Some actions need two scopes: approving a package held because its name
belongs to someone else, and `garner sync --ignore-age`, both need
`publish` and `admin`. An `admin` token does not bypass
[name ownership](ownership.md): it cannot upload under another owner's
name.

`garner init` prints an initial token named `admin`, with owner `admin`
and all three scopes. Create others with
`garner token create --name <name> --owner <label> --scope <scopes>`,
list them with `garner token list`, and revoke one with
`garner token revoke <name>`. The secret is shown once; garner stores only
its sha256. These commands work on the database directly when garner is
stopped, and through the API with `--url` and an admin token while it
runs.

Clients send the token as `<name>:<secret>`: in `GARNER_TOKEN` for the
CLI, as HTTP Basic credentials for the API, and as the user name and
password of `docker login`.

### Tokens only over HTTPS

garner speaks plain HTTP and expects a reverse proxy to terminate TLS. To
keep tokens off the network in clear text, it accepts an authenticated
request only:

- from loopback, or
- from an address in `server.trusted_proxies`, with the header
  `X-Forwarded-Proto: https`.

Anything else gets `403`, before the token is even checked. A proxy on the
same host connects over loopback, but then must set `X-Forwarded-Proto`,
which must say `https` too. Only trusted proxies' `X-Forwarded-*` headers
are believed.

The CLI applies the same rule from its side: it sends a token only over
https or to a loopback address, so a mistyped `http://` URL does not put
the token on the network before the server refuses it.

For an isolated lab, `server.allow_insecure_auth: true` on the server and
`GARNER_INSECURE_AUTH=1` for the CLI turn the rule off.

### The registry's login

Container clients use the OCI token flow: `GET /v2/` without credentials
answers `401` with a `Bearer` challenge pointing at `/v2/token`. An
anonymous client gets a token that allows pulling; `docker login` with a
garner token gets one that carries those credentials, checked again on
every request. A wrong token makes `docker login` fail.

### Secrets

garner never takes a secret in the configuration file itself, so the
file can live in git. Each secret is read from a file, and tokens also
from an environment variable:

| Secret | Keys |
|---|---|
| Forge token | `forges[].token_file` or `token_env` |
| Registry password or token | `registries[].token_file` or `token_env` |
| Signing key | `signing.keys[].key_file` |
| Signing key passphrase | `signing.keys[].passphrase_file` |

Under systemd, hand them to garner with `LoadCredential=` in a drop-in
(`sudo systemctl edit garner`):

```ini
[Service]
LoadCredential=github:/etc/garner/secrets/github.token
LoadCredential=dockerhub:/etc/garner/secrets/dockerhub.token
LoadCredential=signing-key:/etc/garner/secrets/2027.asc
```

and point the keys at the copies systemd makes:

```yaml
forges:
  - name: github
    type: github
    token_file: /run/credentials/garner.service/github
signing:
  keys:
    - key_file: /run/credentials/garner.service/signing-key
```

This is the recommended setup. The files in `/etc/garner/secrets/` can
then be readable by root only: systemd reads them and gives garner a
private copy that only the service sees, so the `garner` user, and
anything else running as it, never needs access to the originals. For
secrets encrypted at rest, systemd's `LoadCredentialEncrypted=` with
`systemd-creds encrypt` works the same way.

**When garner reads them.** Tokens are read at start and at every reload,
signing keys likewise. With `LoadCredential=`, though, systemd copies the
files only when the service starts. To rotate a secret, replace the file
and **restart** garner; a reload would still read the old copy. Without
`LoadCredential=`, replacing the file and reloading is enough.

In containers and on Kubernetes, mount each secret as a file and point
the `*_file` key at it.

### What anonymous readers see

Without a token, anyone who reaches garner can read:

- the repositories and images themselves;
- the UI, including the health of every source and the number of open
  conflicts;
- `GET /api/v1/status`, with source names and their last errors;
- `GET /api/v1/packages`, `/api/v1/owners` and `/api/v1/snapshots`;
- `/metrics`, unless it is moved to its own address with `metrics.listen`.

This suits an internal network. It is why garner is not ready for
internet-facing use: put it where only the hosts that need it can reach
it.

## Why it works this way

- **The network is the boundary for reads.** apt, dnf and docker hosts
  should not need credentials to install software from their own
  repository, and every package they get is signed or addressed by
  digest.
- **Owner labels, not token identities.** Tokens can be rotated and
  revoked without moving any names; the label is what owns them.
- **Refuse before checking.** Rejecting plain-HTTP requests from untrusted
  addresses before looking at the token means a leaked request never tells
  an eavesdropper whether the token was valid.

## Limits

- There are no user accounts, SSO or OIDC federation yet: CI pipelines use
  long-lived tokens. Both are planned.
- There is no audit log yet. garner logs every delivery with its owner,
  and removals, approvals, ownership and token changes with the token's
  name, but keeps none of it in the database.
- Reads cannot be restricted per host or per package.

## See also

- [Name ownership](ownership.md)
- [Getting started](../getting-started.md): creating the first tokens
- Configuration: [`server`](../reference/configuration.md#server)
