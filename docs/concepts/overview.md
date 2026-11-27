# How garner works

garner is one process, `garner serve`, with one configuration file, a
SQLite database and a directory on disk. It takes packages and images in,
decides where each belongs, and publishes everything as one signed,
read-only tree that apt, dnf and container clients read.

```
   uploads (API, CLI)                    forge sync
   image pushes (/v2/)                   registry sync
              \                             /
               v                           v
          +-------------------------------------+
          |               ingest                |  parse, verify, ownership,
          +-------------------------------------+  conflicts, rpm re-signing
                 |                       |
                 v                       v
           blob store               database
                 \                       /
                  v                     v
          +-------------------------------------+
          |              publish                |  render and sign metadata,
          +-------------------------------------+  switch to the new snapshot
                            |
                            v
                  public/ on disk  -->  garner's own server,
                                   -->  or a reverse proxy reading it
```

## The parts

**Formats.** garner serves `.deb` packages as an apt repository, `.rpm`
packages as dnf repositories, and container images as an OCI registry. A
format is turned on by its section in the configuration (`deb:`, `rpm:`,
`oci:`). The rest of garner treats them alike: the same ingest, ownership,
publishing and retention apply to all three. See
[deb](formats/deb.md), [rpm](formats/rpm.md) and
[container images](formats/oci.md).

**Ingest.** Every package or image goes through the same steps on its way
in, whether it was uploaded, pushed or synced: garner reads its name and
version from the file itself, checks it, checks who owns the name, and
stores it. A file that would break a repository for its clients is
refused at this point rather than at publish. See [ingest](ingest.md).

**Name ownership.** Every package and image name has one owner: the team
whose token first delivered it, or a sync source that reserves it. Nobody
else can publish under that name, so a CI pipeline cannot replace another
team's package by accident or on purpose. See [ownership](ownership.md).

**Publishing and snapshots.** garner never changes the published tree in
place. Each publish writes a complete new snapshot of the metadata and
switches to it in one step, so clients always see one consistent,
signed repository. Old snapshots stay for rollback and pinning. See
[publishing](publishing.md).

**Retention and garbage collection.** garner keeps the newest few versions
of each package (three by default) and every tagged image, and deletes
files once no kept snapshot refers to them. See
[retention and GC](retention-gc.md).

**Signing.** Clients trust one key, the repository's. garner signs the apt
and dnf metadata with it and re-signs every rpm, so hosts never need the
keys of the projects whose packages they install. garner checks those
projects' checksums itself when it syncs. See [signing](signing.md).

**Sources.** A source is something garner watches for new releases: a
repository or a whole organization on GitHub, GitLab or Forgejo, or an
image on a registry such as ghcr.io or Docker Hub. garner polls each
source, takes the assets or tags its rules select, and ingests them like
uploads. See [sources](sources.md), [forges](forges.md) and
[registries](registries.md).

**Security.** Reading is open to anyone who can reach garner. Every write
needs a token with the right scope, and tokens are accepted only over
HTTPS through a trusted proxy or from the same host. See
[security](security.md).

**Storage.** Packages are stored once, by their content hash, however many
repositories list them. The database holds what garner knows about them;
the files on disk are the repository itself. See [storage](storage.md).

**Verification.** `garner verify` reads the published tree the way clients
do and compares it with the database, for use after an upgrade, a restore
or a client error. See [verification](verification.md).

**The UI.** garner's front page, at `server.public_url`, shows what the
repository holds and how to use it from a host.

## Configuration and state

The configuration file holds what people decide: formats, sources, rules
and limits. It fits a GitOps workflow: keep it in git, check it in CI with
`garner config validate`, and apply it with `garner reload` or `SIGHUP`. A
file that does not validate is refused, and garner keeps running on the
last good one. The command line never changes what the file defines.

The database holds what garner learns while it runs: packages, owners,
tokens, snapshots and the progress of each source. Every key is listed in
the [configuration reference](../reference/configuration.md).

## When garner is down

The published tree is plain files. If a reverse proxy serves it straight
from disk, apt and dnf clients keep working while garner restarts or is
stopped; only uploads, sync and image pulls wait for it. Under systemd,
garner also keeps its listening socket open across restarts, so requests
to garner itself wait instead of failing.

The UI keeps working too, since it is a file in the snapshot. Only its
live parts, the health of sources and open conflicts, come from garner
(`GET /api/v1/status`); while garner is down they say the status is
unavailable.
