# Monitoring

garner exposes health endpoints and Prometheus metrics, and logs to
stdout. It sends no notifications itself: alerting belongs to Prometheus
or whatever watches it. This guide lists what to watch and suggests
alerts.

## Health endpoints

| Endpoint | Answers `200` when |
|---|---|
| `/healthz` | the process is running |
| `/readyz` | the database answers, the storage directory exists, and a current snapshot exists; otherwise `503` with the problems |

Use `/readyz` for load balancer and Kubernetes readiness checks.

The README's proxy examples pass only `/api/` and `/v2/` to garner, so
these endpoints and `/metrics` are not reachable through the proxy. Query
them on garner's own address, or serve `/metrics` on a separate one:

```yaml
metrics:
  listen: "127.0.0.1:9090"
```

## Metrics

| Metric | What it shows |
|---|---|
| `garner_publish_last_success_timestamp_seconds` | When the last publish succeeded |
| `garner_publish_duration_seconds` | How long publishes take |
| `garner_publish_errors_total` | Failed publishes; garner retries them, except one asked for with `garner publish`, which reports the error |
| `garner_sync_last_success_timestamp_seconds{source}` | When each source last synced without error |
| `garner_sync_errors_total{source}` | Failed sync runs per source |
| `garner_sync_packages_added_total{source}` | Packages each source delivered |
| `garner_sync_releases_deferred{source}` | Releases waiting for `min_release_age` |
| `garner_forge_ratelimit_remaining{forge}` | API requests left before the forge's rate limit |
| `garner_conflicts_open` | Packages held for an ownership decision |
| `garner_conflicts_total{kind}` | Every conflict recorded, by kind; checksum mismatches included |
| `garner_packages{format,distribution,arch}` | Packages published |
| `garner_uploads_total{result}`, `garner_upload_bytes_total` | Uploads and their size |
| `garner_storage_bytes` | Disk used by the storage directory |
| `garner_snapshots` | Snapshots kept |
| `garner_signing_key_expiry_days` | Days until the signing key or its signing subkey expires, when it has an expiry |
| `garner_signing_resign_pending` | 1 while rpm packages still need re-signing with the current key |
| `garner_http_requests_total{route,code}` | Requests by route and status |

## Suggested alerts

```yaml
groups:
  - name: garner
    rules:
      - alert: GarnerDown
        expr: up{job="garner"} == 0
        for: 5m
      - alert: GarnerPublishFailing
        expr: increase(garner_publish_errors_total[30m]) > 0
          and time() - garner_publish_last_success_timestamp_seconds > 1800
      - alert: GarnerSyncStale
        # adjust to a few poll intervals
        expr: time() - garner_sync_last_success_timestamp_seconds > 4 * 3600
      - alert: GarnerConflict
        # a checksum mismatch or an asset skipped for good
        expr: increase(garner_conflicts_total[1h]) > 0
      - alert: GarnerHeldPackages
        expr: garner_conflicts_open > 0
        for: 1d
      - alert: GarnerRateLimitLow
        expr: garner_forge_ratelimit_remaining < 100
        for: 15m
      - alert: GarnerSigningKeyExpiring
        expr: garner_signing_key_expiry_days < 30
      - alert: GarnerResignStuck
        expr: garner_signing_resign_pending == 1
        for: 1h
```

`GarnerConflict` matters most for security: a checksum mismatch on a
synced asset means the file garner downloaded is not the one the project
published.

## Checking the repository itself

The metrics tell you garner is running; `garner verify` tells you clients
will not fail. Run it on a schedule with a token that has `publish`, for
example from a systemd timer or CI, and alert when it exits non-zero:

```sh
garner verify --wait 5m
```

See [verification](../concepts/verification.md).

## Logs

garner logs to stdout, as JSON by default (`log.format: text` for
reading by eye), at `log.level` and above. Under systemd they are in
`journalctl -u garner`. Every delivery is logged with its owner, and removals, approvals,
ownership changes and token changes with the name of the token that made
them.

## See also

- [Troubleshooting](troubleshooting.md)
- Configuration: [`metrics`](../reference/configuration.md#metrics),
  [`log`](../reference/configuration.md#log)
