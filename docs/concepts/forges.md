# Forges

A forge is a configured instance of GitHub, GitHub Enterprise Server,
GitLab or Forgejo (and Gitea, which garner treats as Forgejo). Forge
[sources](sources.md) name a forge and sync release assets from it.

```yaml
forges:
  - name: github
    type: github                # github.com; set url for Enterprise Server
    token_env: GITHUB_TOKEN
  - name: gitlab
    type: gitlab
    url: https://gitlab.example.com
    token_file: /run/credentials/garner.service/gitlab
```

## How it works

### What a source can sync

| Source | GitHub, GHES | Forgejo, Gitea | GitLab |
|---|---|---|---|
| One repository | `repo: owner/name` | `repo: owner/name` | `repo: group/sub/project` |
| Every repository of an owner | `org:` | `org:` | `group:`, with subgroups unless `recursive: false` |

For `org` and `group` sources, garner fetches the list of repositories on
every poll, so new ones are included automatically. `include` and
`exclude` filter them by name. Archived repositories and forks are
skipped unless `archived: true` or `forks: true`. On GitLab, projects
shared into the group from other namespaces are not synced.

Repositories without matching assets are listed in `garner status --json`, so a
typo in an asset rule shows up.

### How each forge differs

| | GitHub, GHES | Forgejo, Gitea | GitLab |
|---|---|---|---|
| Assets | Release assets, downloaded through the API, which works for private repositories | Release attachments | Release links, often to the Generic Package Registry or another host |
| Asset digest | Yes, on recent releases; used instead of checksum files | No | No |
| Asset upload times | Yes | Yes | No: `min_release_age` counts from the release only |
| Release time | `published_at` | `published_at` | The later of `created_at` and `released_at`, since `released_at` can be set to a past date |

### Credentials

A token is optional for public repositories and required for private ones.
garner sends it only to the forge's own scheme, host and port. A release
link that points to another host is fetched without the token, and if
that fails, the asset fails with an error. For a private repository on
Forgejo or GitLab, set `url` to the address the forge puts in its links
(Forgejo's `ROOT_URL`, GitLab's `external_url`), or downloads fail without
the token.

Give tokens read-only access. Secrets are never written in the
configuration file: use `token_file` or `token_env`; under systemd,
`LoadCredential=` is recommended (see [secrets](security.md#secrets)).

### Rate limits

Forges limit how many API requests a client makes. garner honours
`Retry-After` and the rate-limit headers, makes at most `max_concurrent`
requests to one forge at a time (4 by default), and reports the remaining
quota as `garner_forge_ratelimit_remaining`.

On GitHub, a token raises the limit from 60 requests an hour per IP
address to 5,000 an hour for the token's user. 60 covers a few
repositories, not an organization. Requests for releases that have not
changed do not count against GitHub's limit when they carry a token.

### Network

- TLS verification cannot be turned off. For an internal CA, set
  `ca_file`.
- `HTTPS_PROXY` and `NO_PROXY` apply by default; `proxy` sets a proxy for
  one forge.

## Why it works this way

- **Credentials stay on their host.** A release link can point anywhere;
  sending a token only to the forge itself keeps a crafted link from
  collecting it.
- **No TLS opt-out.** Downloads are trusted on the strength of TLS and
  checksums together; turning TLS off would leave the checksums as the
  only check, and they come over the same connection.

## Limits

- Sync has been tested against GitHub, and against test servers that
  mimic the Forgejo, GitLab and GitHub Enterprise Server APIs, not yet
  against real instances of those.
- Releases are listed newest first and garner stops at the last one it
  processed. A release created long before it is published, such as a
  draft kept for weeks, can sort below that point and be missed.

## See also

- [Sources](sources.md)
- [Syncing sources](../guides/sync-sources.md)
- Configuration: [`forges`](../reference/configuration.md#forges)
