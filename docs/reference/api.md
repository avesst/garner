# HTTP API

garner's API lives under `/api/v1/`. The `garner` CLI is a client of it:
every API command in `garner(1)` maps to one endpoint below, and `--json`
prints the response as it comes. The container registry at `/v2/`
follows the [OCI distribution spec](https://github.com/opencontainers/distribution-spec)
and is described in [container images](../concepts/formats/oci.md).

## Conventions

- **Authentication:** HTTP Basic, with the token's name as the user and
  its secret as the password (`curl -u "name:secret"`). Accepted only over
  HTTPS through a trusted proxy, or from loopback; see
  [security](../concepts/security.md).
- **Bodies** are JSON, except the package file of an upload.
- **Errors** are JSON: `{"error": "message"}`.

## Status codes on every endpoint

Besides the codes listed per endpoint, any endpoint can answer:

| Code | When |
|---|---|
| `401` | The endpoint needs a token and the request has none, or the token is unknown or revoked. The answer carries `WWW-Authenticate: Basic`. |
| `403` | The token arrived other than over HTTPS through a trusted proxy or from loopback, or it lacks the endpoint's scope. Checked before the token itself. |
| `404` | No such endpoint under `/api/`. |
| `405` | The path exists, but not for this method. |
| `500` | An error inside garner, such as a database or disk error. The message says to see the server log, which has the cause. |

`401` and `403` apply only to endpoints that need a scope.

## Endpoints

| Method and path | Scope | Purpose |
|---|---|---|
| `PUT /api/v1/upload` | `upload` | Upload one package |
| `GET /api/v1/status` | none | Version, configuration, snapshot, conflicts, sources |
| `GET /api/v1/packages` | none | List packages |
| `DELETE /api/v1/packages/{name}` | `admin` | Remove packages or images |
| `POST /api/v1/packages/{id}/approve` | `publish` (and `admin` for an ownership hold) | Publish a held package |
| `GET /api/v1/owners` | none | List name owners |
| `PUT /api/v1/owners/{name}` | `admin` | Set a name's owner |
| `POST /api/v1/sources/{name}/sync` | `publish` (and `admin` with `ignore_age`) | Poll a source now |
| `GET /api/v1/conflicts` | `publish` | List conflicts |
| `GET /api/v1/snapshots` | none | List kept snapshots |
| `POST /api/v1/snapshots/{id}/rollback` | `publish` | Publish a snapshot's packages again |
| `POST /api/v1/publish` | `publish` | Publish now |
| `POST /api/v1/verify` | `publish` | Check the published tree |
| `POST /api/v1/gc` | `admin` | Run garbage collection |
| `POST /api/v1/reload` | `admin` | Reload the configuration |
| `GET /api/v1/tokens` | `admin` | List tokens |
| `POST /api/v1/tokens` | `admin` | Create a token |
| `DELETE /api/v1/tokens/{name}` | `admin` | Revoke a token |

Outside the API, `GET /healthz` answers `200` while the process runs, and
`GET /readyz` answers `200`, or `503` with the problems, see
[monitoring](../guides/monitoring.md). `GET /metrics` serves Prometheus
metrics.

Each endpoint below lists its parameters: in the **path**, the **query**
string, or the JSON **body**.

### `PUT /api/v1/upload`

Uploads one `.deb` or `.rpm` and places it in a suite or release.

| Parameter | In | Required | Meaning |
|---|---|---|---|
| (the file) | body | yes | The package file itself, sent as the raw request body, not as a form. garner reads its format, name, version and architecture from its contents; the file name does not matter. |
| `suite` | query | no | The deb suite a `.deb` goes into. Must be one of `deb.suites`. Ignored for an `.rpm`. |
| `release` | query | no | The rpm release an `.rpm` goes into. Must be one of `rpm.releases`. Ignored for a `.deb`. |

Without `suite` or `release`, the package goes into `stable` if it is
configured, else the first configured suite or release. Setting both lets
a script send `.deb` and `.rpm` files to the same URL. Only the first of a
repeated parameter counts: to place a package in two suites, upload it
twice.

```sh
curl -fsS -u "$GARNER_TOKEN" -T tool_1.4.0_amd64.deb \
    "https://pkg.example.com/api/v1/upload?suite=stable"
```

| Code | When |
|---|---|
| `200` | Stored and placed: `{"package": {...}, "existing": false}`. `existing` is true when the same file was already there. |
| `400` | Not a valid package, a package that would break clients, or a suite, release or architecture that is not configured; then the body also lists the configured ones under `configured`. |
| `409` | The name belongs to another owner, or this version is already in the target with different content. |
| `413` | Larger than `storage.max_package_size`. |

### `GET /api/v1/status`

garner's own state, as `garner status` and the UI show it. No parameters.

```json
{
  "version": "0.2.2",
  "config_hash": "fc723bb75ef9...",
  "config_commit": "741cb6d",
  "current_snapshot": "01M8EZQW1C6M09NYFFBPGS0AC4",
  "published_at": "2026-11-27T17:08:42Z",
  "open_conflicts": 0,
  "held_packages": 0,
  "sources": [
    {
      "name": "tool",
      "kind": "forge",
      "last_run": "...",
      "last_success": "...",
      "last_error": "",
      "repos_without_assets": [],
      "skipped_assets": [],
      "deferred_releases": [{"repo": "example/tool", "tag": "v1.2.4", "eligible_at": "..."}]
    }
  ]
}
```

`kind` is `forge` or `registry`. Empty fields are left out.

| Code | When |
|---|---|
| `200` | Always, unless garner fails internally. |

### `GET /api/v1/packages`

Lists packages and where they are placed: one entry per package and
suite, release or image name. Every parameter narrows the list, and they
combine; values must match exactly.

| Parameter | In | Required | Meaning |
|---|---|---|---|
| `name` | query | no | Package or image name, such as `tool` or `team/tool`. |
| `format` | query | no | `deb`, `rpm` or `oci`. |
| `arch` | query | no | Architecture as the package states it: `amd64`, `x86_64`, `all`, `noarch`. |
| `distribution` | query | no | Suite or release, such as `stable` or `el9`. Images are all in `default`. |
| `state` | query | no | Only `active` (published), `held` (waiting for approval) or `removed` (removed by hand or by retention). |
| `all` | query | no | `1` to list every state. Ignored when `state` is set. |

Without `state` or `all`, active and held packages are listed. Each entry
has `id`, `format`, `name`, `version`, `arch`, `distribution`, `state`,
`sha256` and its origin. `id` is what
[approve](#post-apiv1packagesidapprove) takes.

| Code | When |
|---|---|
| `200` | The list, possibly empty. |

### `DELETE /api/v1/packages/{name}`

Takes a package or image out of future snapshots. It stays stored, and
listed with state `removed`, until GC deletes it. Every query parameter
narrows what is removed; without any, every active and held placement of
the name goes.

| Parameter | In | Required | Meaning |
|---|---|---|---|
| `name` | path | yes | Package or image name. Encode a `/` in an image name as `%2F`: `/api/v1/packages/team%2Ftool`. The CLI does this itself. |
| `version` | query | no | One version: the full version as `packages` lists it (`1:2.0-1`), or the version without epoch and release (`2.0`). For an image, its digest (`sha256:...`). |
| `arch` | query | no | One architecture. |
| `distribution` | query | no | One suite or release. |
| `format` | query | no | `deb`, `rpm` or `oci`. Without it, a package and an image of the same name are both removed. |

| Code | When |
|---|---|
| `200` | The removed packages. |
| `404` | Nothing matched. |

### `POST /api/v1/packages/{id}/approve`

Publishes a held package. No body.

| Parameter | In | Required | Meaning |
|---|---|---|---|
| `id` | path | yes | The package's ID, from [`GET /api/v1/packages`](#get-apiv1packages) with `state=held`. |

| Code | When |
|---|---|
| `200` | The approved package. |
| `403` | The package is held because its name belongs to another owner, and the token lacks `admin`. |
| `404` | No held package with that ID. |
| `409` | Another file of the same version is already in its distribution. |

### `GET /api/v1/owners`

Every owned name, with its owner, how it was assigned (`first` delivery,
a source's `config` reservation, or `manual`), and when. No parameters.

| Code | When |
|---|---|
| `200` | The list. |

### `PUT /api/v1/owners/{name}`

Hands a name to another owner.

| Parameter | In | Required | Meaning |
|---|---|---|---|
| `name` | path | yes | The package or image name. |
| `owner` | body | yes | The new owner: a token owner label such as `team-b`, or `source:<name>`. |
| `namespace` | body | no | `os` for package names (the default) or `oci` for image names. |

```json
{"owner": "team-b", "namespace": "oci"}
```

| Code | When |
|---|---|
| `200` | The new owner record. |
| `400` | The body is not JSON or has no `owner`. |

### `POST /api/v1/sources/{name}/sync`

Polls a source now instead of at its next interval. Without a body, that
is all it does. With `ignore_age`, it also lets one release skip the
source's `min_release_age`, which needs `admin` as well as `publish`.

| Parameter | In | Required | Meaning |
|---|---|---|---|
| `name` | path | yes | The source's `name` from the configuration. |
| `ignore_age.tag` | body | with `ignore_age` | The release tag, or for a registry source the image tag, to let through. |
| `ignore_age.repo` | body | for org and group sources | The repository the tag belongs to, as the source's last poll listed it (`example/tool`). A single-repository or registry source knows its own. |

```json
{"ignore_age": {"tag": "v1.2.4", "repo": "example/tool"}}
```

The override covers the release and assets as they are when it is made: a
release re-created under the same tag, or an asset uploaded later, waits
again. The body is strict: unknown keys and trailing data are refused.

| Code | When |
|---|---|
| `202` | The poll started, or the override was stored. An override is stored even when the poll cannot start, and applies on the next one. |
| `400` | The body is malformed, has no `tag`, lacks `repo` for an org or group source, or names a repository the source's last poll did not list. |
| `403` | `ignore_age` without the `admin` scope. |
| `404` | No source of that name, or no sources configured. |
| `409` | `ignore_age` for a source without `min_release_age`, or for a tag already processed. |

### `GET /api/v1/conflicts`

Lists conflicts.

| Parameter | In | Required | Meaning |
|---|---|---|---|
| `all` | query | no | `1` to list every conflict ever recorded: resolved ones, and sync conflicts such as checksum mismatches, which are recorded resolved. Without it, only open conflicts: ownership holds waiting for a decision. |

| Code | When |
|---|---|
| `200` | The list, possibly empty. |

### `GET /api/v1/snapshots`

Each kept snapshot's `id`, creation time, configuration hash and whether
it is current. No parameters.

| Code | When |
|---|---|
| `200` | The list. |

### `POST /api/v1/snapshots/{id}/rollback`

Publishes a new snapshot with the packages and image tags of an earlier
one, and places them so the next publish keeps them. No body.

| Parameter | In | Required | Meaning |
|---|---|---|---|
| `id` | path | yes | A kept snapshot's ID, from [`GET /api/v1/snapshots`](#get-apiv1snapshots). |

| Code | When |
|---|---|
| `200` | The new snapshot. |
| `404` | No kept snapshot with that ID. |

### `POST /api/v1/publish`

Publishes now, without waiting for the 10-second debounce. No parameters.

| Code | When |
|---|---|
| `200` | The new snapshot. |

### `POST /api/v1/verify`

Checks the published tree against the database, as `garner verify` does.
Both parameters make it refuse with `409` instead of checking, so a
caller can retry until the state it expects is live.

| Parameter | In | Required | Meaning |
|---|---|---|---|
| `ready` | query | no | `1`: refuse while a publish is pending, so the check covers the latest changes. |
| `config_hash` | query | no | The start of a configuration file's sha256, at least 7 hex digits (`sha256sum garner.yaml`): refuse while another configuration is loaded. |

| Code | When |
|---|---|
| `200` | The report: errors and warnings. Problems found are in the report, not the status code. |
| `400` | `config_hash` shorter than 7 hex digits. |
| `409` | A publish is pending (`ready=1`), or another configuration is loaded (`config_hash`). |

### `POST /api/v1/gc`

Runs garbage collection now. No parameters.

| Code | When |
|---|---|
| `200` | What GC deleted. |

### `POST /api/v1/reload`

Reloads the configuration file, like `SIGHUP`. No parameters.

| Code | When |
|---|---|
| `200` | `{"config_hash": "..."}` of the configuration now loaded. |
| `400` | The file is invalid, or the change needs a restart; the message says which. The running configuration stays. |

### `GET /api/v1/tokens`

Every token with its name, owner, scopes, and when it was created, last
used and, if it was, revoked. Revoked tokens stay in the list. Secrets
are never returned. No parameters.

| Code | When |
|---|---|
| `200` | The list. |

### `POST /api/v1/tokens`

Creates a token.

| Parameter | In | Required | Meaning |
|---|---|---|---|
| `name` | body | yes | Unique name, used as the user name in Basic auth and in logs. Letters, digits, `.`, `_` and `-`. |
| `owner` | body | yes | Owner label: the names this token delivers first become this owner's. Tokens of one team share a label. Letters, digits, `.`, `_` and `-`, not starting with `source`. |
| `scopes` | body | yes | One or more of `upload`, `publish` and `admin`. They do not imply each other. |

```json
{"name": "ci-tool", "owner": "team-a", "scopes": ["upload"]}
```

| Code | When |
|---|---|
| `201` | The token and its `secret`, shown once. |
| `400` | The body is not JSON, a field is invalid, or a token of that name exists. |

### `DELETE /api/v1/tokens/{name}`

Revokes a token. Requests with it fail from then on; the names its owner
label owns stay owned.

| Parameter | In | Required | Meaning |
|---|---|---|---|
| `name` | path | yes | The token's name. |

| Code | When |
|---|---|
| `200` | `{"revoked": "<name>"}`. |
| `404` | No active token of that name. |

## See also

- `garner(1)`: the CLI over this API
- [Security and tokens](../concepts/security.md)
