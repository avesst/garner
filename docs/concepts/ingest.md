# Ingest

Ingest is how a package or image gets into garner. Whatever the way in, an
upload, a `docker push` or a sync from a forge or registry, it goes
through the same checks, so a package is accepted or refused for the same
reasons however it arrives.

## How it works

### The ways in

- **Upload.** `garner upload` or `PUT /api/v1/upload` with a `.deb` or
  `.rpm`, using a token with the `upload` scope. The package goes into the
  deb suite or rpm release named with `--suite` or `--release`, or else
  into `stable` (or the first configured one, without `stable`).
- **Image push.** `docker push` or `podman push` to garner's registry at
  `/v2/`, logged in with a token that has `upload`. See
  [container images](formats/oci.md).
- **Sync.** garner polls its [sources](sources.md) and ingests the release
  assets or image tags their rules select.

### The steps

1. **Store.** The file is written to the blob store under its sha256 and
   flushed to disk before anything refers to it. A file garner already has
   is not stored twice.
2. **Read.** garner finds the format from the file's content, never its
   name, and reads the package's name, version and architecture from its
   own metadata. The file name you upload under does not matter.
3. **Check that clients can read it.** garner refuses a package that would
   break the repository for every client of it, not just itself (see
   below).
4. **Check the architecture.** A package for an architecture that is not
   configured is refused.
5. **Check the owner.** The name must belong to whoever delivers it, or be
   free. See [name ownership](ownership.md).
6. **Check for conflicts.** The same name, version and architecture in the
   same suite or release with different content is refused; the same file
   again is accepted and changes nothing.
7. **Re-sign rpms.** An rpm is re-signed with the repository key now, so a
   signing problem is reported to whoever delivered it rather than
   failing a later publish. See [signing](signing.md).
8. **Record and publish.** garner records the package, places it in its
   suite or release, and asks for a [publish](publishing.md).

### Packages that would break clients

Some packages are valid enough to install by hand but would make apt or
dnf fail on the whole repository once listed. garner refuses them at
ingest, with a message naming the field:

- **deb:** a relationship field (`Depends`, `Breaks`, `Provides` and the
  others apt reads) that does not parse. apt rejects the whole `Packages`
  file over one such field.
- **rpm:** control characters, such as the colour codes of a generated
  changelog, in the rpm's header text. The repodata XML cannot hold them,
  and `createrepo_c` then fails for the whole repository. garner refuses
  the package rather than rewriting its header.
- **both:** a name, version and architecture so long that the file name
  garner gives it in the pool would exceed 200 bytes.

### Conflicts and holds

When a delivery fails a check, what happens depends on how it arrived:

- **An upload** gets an error and nothing is kept: `409` with the owner's
  name for an ownership or content conflict, `400` for an invalid package
  or an unconfigured suite, release or architecture, `413` for a file over
  `storage.max_package_size`. A push to another owner's image name is
  refused with the registry error `DENIED`, naming the owner, before any
  layer is sent.
- **A sync** cannot be answered, so garner records a **conflict** instead.
  A package whose name belongs to someone else is stored but **held**: not
  published, and claiming nothing, until someone decides. Other problems,
  such as a checksum mismatch or two assets that would be the same
  package, skip the asset and are recorded for the record.

`garner conflicts list` shows them. `garner packages list --state held`
lists held packages with their IDs, and `garner approve <id>`
publishes it. Approving a package under another owner's name needs the
`admin` scope as well as `publish`. `garner remove` drops it instead.

### Manual approval

A source with `approval: manual` holds everything it delivers, conflict or
not, until `garner approve`. Use it for sources you want to review before
hosts install from them. Uploads and pushes are never held: they come
with a token, which is the approval.

## Why it works this way

- **One path for every way in.** Ownership, conflicts and retention are
  only trustworthy if nothing can go around them. A sync cannot deliver
  what an upload could not, and the other way round.
- **Content decides, not names.** File names from forges are often wrong
  or inconsistent. Reading the package's own metadata means the repository
  lists what the package says it is, the same thing apt and dnf will see.
- **Refuse early.** A package that breaks a repository would otherwise
  surface at publish, where it stops every format from publishing, and
  with nobody to tell. At ingest, the error goes to the pipeline or the
  sync status that delivered it.
- **Versions are immutable.** Once a version is in a suite, the same
  version with other content is refused. Hosts that already installed it
  would otherwise have a different file under the same version, and apt
  and dnf would never update them.

## Limits

- Uploads take `.deb` and `.rpm` only. Images arrive by push or sync, not
  through `PUT /api/v1/upload`.
- A new package is visible to clients after the next
  [publish](publishing.md), about 10 seconds later.
- Packages stored before a check was added are not checked again.

## See also

- [Name ownership](ownership.md)
- [Sources](sources.md): what sync delivers, and its own checks on assets
- [Signing](signing.md): rpm re-signing
- Configuration: [`storage.max_package_size`](../reference/configuration.md#storage),
  [`defaults.approval`](../reference/configuration.md#defaults)
