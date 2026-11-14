# garner implementation plan

Status: draft, 2026-11-14. The design is in `design.md`; this file only says
in which order to build it and when each piece is done. Update it as slices
land.

## Project basics

- Repository: `github.com/avesst/garner` (Forgejo). May move to GitHub later.
- Go module path: `github.com/avesst/garner`. A move means one rewrite of the
  import paths; acceptable while garner is an application, not a library.
- Go: 1.26 (`go 1.26` in `go.mod`).
- CI: Forgejo Actions. Its workflow syntax is GitHub Actions compatible, so a
  move to GitHub keeps the workflows.
- Integration tests run real clients in containers (Docker locally and in CI).

## MVP step 1, as slices

Deb first, end to end, then rpm. Each slice ends in something that runs.

| # | Slice | Done when |
|---|---|---|
| 1 | Skeleton: module, `cmd/garner`, CI running `go vet` and `go test` | CI passes on push |
| 2 | Configuration: load, validate, `config validate`, reload on `SIGHUP` (section 5) | Table tests cover valid and invalid files; an invalid reload keeps the old config |
| 3 | Database: SQLite, goose migrations, `flock` guard (sections 6, 12) | Migrations apply on SQLite; CI also applies them to PostgreSQL; a second process refuses to start |
| 4 | Blob store, deb parsing, dpkg version comparison (sections 6, 7) | dpkg's own version test vectors pass; storing the same file twice is a no-op |
| 5 | Signing: `init` generates a key; clearsign `InRelease` (section 10) | `sqv` and `gpgv` verify the output |
| 6 | Publish deb: `Packages`, `Release`, hashed pool, by-hash with history, snapshot, `current` swap, landing page, client files, built-in server (sections 4, 9, 14) | `apt update && apt install` works in `debian:13` and `ubuntu:26.04` containers; an `apt update` with a publish forced between `InRelease` and `Packages` succeeds |
| 7 | Snapshot list, rollback, GC (section 9) | A rollback is a new snapshot; GC removes only unreferenced blobs and respects `gc_grace` |
| 8 | rpm: `createrepo_c`, re-signing with `rpmsign`, repodata history (sections 4, 9, 10) | `dnf install` works in `almalinux:10` and `fedora` containers with `gpgcheck` and `repo_gpgcheck` on |

## Later MVP steps

Planned in slices when step 1 is done.

- Step 2: upload API and CLI, tokens, name ownership.
- Step 3: GitHub sync.
- Step 4: Forgejo/Gitea, GitLab and GitHub Enterprise Server clients.
- Step 5: metrics, health endpoints, container image, packages for garner.

## Decisions to make when their slice arrives

| Decision | Needed by | Current recommendation |
|---|---|---|
| CLI library: standard `flag` or a framework | Slice 1 | Standard library unless subcommand handling gets awkward |
| Snapshot retention default | Slice 7 | By age with a minimum count, plus `snapshot keep ID` |
| rpm when its tools are missing | Slice 8 | rpm off, with a clear startup error |
| One deb in several suites | Step 2 | `suite: [noble, jammy]` and repeated `?suite=` |
| Packaging garner (nfpm or GoReleaser; `.rpm` too?) | Step 5 | nfpm |
| Default deployment, backup and restore, rpm `Signer` limit | Before release | Document; proxy serves `public/` |
| License | Before the first public release | Open |

## Dependencies to approve

Each is asked for before the slice that needs it.

- Named in the design: `modernc.org/sqlite`, `pressly/goose`,
  `ProtonMail/go-crypto`.
- Expected, not yet approved: a YAML library (slice 2), a ULID library or a
  small local implementation (slice 6), an xz writer for `Packages.xz`
  (slice 6).
