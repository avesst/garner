# Name ownership

Every package name and every image name in garner has exactly one owner,
and only that owner can deliver it. This keeps one team, pipeline or sync
source from publishing over another's packages, by mistake or on purpose.

## How it works

### Owners

An owner is one of two things:

- **A token's owner label**, such as `team-platform`, set when the token
  is created with `garner token create --owner`. Ownership follows the
  label, not the token: several tokens can share a label, and revoking or
  rotating a token keeps the team's names.
- **A sync source**, shown as `source:<name>`. A source owns the names it
  delivers or reserves.

### Claiming a name

The first delivery of a name that nobody owns makes its deliverer the
owner. After that, only the owner can deliver that name:

- an upload of a name owned by someone else is refused with `409`, naming
  the owner, and nothing is stored;
- an image push to such a name is refused with `DENIED` before any layer
  is sent;
- a sync that delivers such a name stores the package as **held** and
  records a conflict (see [ingest](ingest.md#conflicts-and-holds)). The
  held package claims nothing, and the owner can still deliver the same
  version.

`garner owner list` shows every name and its owner, and how the owner
got it: by first delivery, by a source's reservation, or set by hand.

### Namespaces

Names are owned per ecosystem. deb and rpm share one namespace, since a
package called `tool` is usually the same project in both, so the owner of
`tool` owns it for deb and rpm alike. Images have a namespace of their
own: owning the package `tool` says nothing about the image `tool`.

An image name is its whole repository path: `team/tool` is one name, and
its owner owns that name only. Owning `team/tool` gives no claim on
`team/other`; only a source's `owns:` glob can reserve every name below
a prefix (see below).

### Reserving names for a source

A source can reserve names ahead of any delivery with `owns:`:

```yaml
sources:
  - name: tool
    forge: github
    repo: example/tool
    owns: ["tool", "tool-*"]
```

From then on only that source can deliver `tool` or any `tool-*`
package, and an upload of one is refused, even before the source has
delivered anything. In a glob, `*` and `?` match any character,
including `/`, so `team/*` reserves every image below `team/`.

A source reserves names only for what it delivers: a forge source
reserves package names, a registry source image names. A forge source and
a registry source can therefore both reserve `tool` without conflict.

Reservations of two sources must never overlap. If any name could match
globs of two sources, `garner config validate` and reload refuse the
file, naming both sources, both globs and an example name that matches
both.

A reservation added later, for a name somebody else already owns, wins
for every future delivery. Packages already delivered under the old owner
stay, and the conflict is reported.

### Names a package replaces

A package can also take over other names on the hosts that install it:
deb's `Replaces` and rpm's `Obsoletes`. dnf applies `Obsoletes` on an
ordinary upgrade, so an owner of `a` shipping `Obsoletes: b` would remove
`b` from hosts. garner therefore checks those names like the package's own
name: each must be free or belong to the same owner, and free ones are
claimed by the deliverer. `Provides` is not checked: virtual names such
as `webserver` have many providers by design.

deb names are compared in lower case, as dpkg compares them.

### Changing an owner

`garner owner set <name> <owner>` hands a package name to another owner.
It needs the `admin` scope. Admin tokens do not bypass ownership
otherwise: an admin uploading a package still needs to own its name.

Approving a held package that another owner's name blocked needs `admin`
as well as `publish`, since it publishes under a name its deliverer does
not own. It does not change the owner.

## Why it works this way

- **Names are what hosts trust.** A host that installed `tool` takes any
  newer `tool` from the repository. Whoever can publish under the name
  controls what those hosts run, so the name is what has to be protected.
- **First delivery claims.** Teams can start publishing without asking an
  administrator first, and from then on their names are safe.
- **Reservations for sources.** A synced project's names should belong to
  the sync, not to whichever pipeline uploads a package of that name
  first.
- **Holds for sync, errors for uploads.** An upload has someone waiting
  for the answer; a sync does not, so its conflicts wait for a person.

## Limits

- Ownership stays when all of a name's packages are removed; it moves only
  with `garner owner set` or a reservation.
- `garner owner set` changes package names only. An image name's owner can
  be changed through the API (`PUT /api/v1/owners/{name}` with
  `"namespace": "oci"`), not from the command line.
- Packages stored before the `Replaces` and `Obsoletes` check was added
  are not checked again.
- One source cannot reserve a pattern while another owns an exception to
  it (`tool-*` except `tool-cli`); that is planned.

## See also

- [Ingest](ingest.md): the checks a delivery goes through
- [Security and tokens](security.md): owner labels and scopes
- [Sources](sources.md)
- Configuration: [`sources[].owns`](../reference/configuration.md#sources)
