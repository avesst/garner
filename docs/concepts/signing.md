# Signing

Hosts that use a garner repository trust one OpenPGP key: the
repository's. garner signs everything apt and dnf check with it, so hosts
never need the keys of the projects whose packages they install. garner
checks those projects' releases itself, when it [syncs](sources.md) them.

## How it works

### The key

`garner init` generates an RSA 4096 key without passphrase and without
expiry when `signing.keys` is empty, at `<storage.path>/signing/key.asc`.
To use a key of your own, point `signing.keys[].key_file` at an armored
private key, with `passphrase_file` if it has a passphrase.

The public keys are published with every snapshot as `/key.asc` (armored)
and `/key.gpg` (binary). The apt client file carries them inline, so an
apt host installs one file and no separate keyring.

### What is signed

| Format | Signed | Checked by |
|---|---|---|
| deb | `InRelease` and `Release.gpg`, the suite's top-level metadata | apt: `InRelease` holds the hashes of the `Packages` files, which hold the hash of every `.deb`. Individual `.deb` files are not signed. |
| rpm | `repomd.xml.asc`, and every rpm package itself | dnf with `repo_gpgcheck=1` checks the metadata, and with `gpgcheck=1` each package's own signature |
| Images | nothing | Clients rely on TLS and pull by digest; see below |

### Re-signing rpm packages

rpm packages carry their own signature, and dnf checks it against the keys
it trusts. garner therefore **re-signs every rpm** with the repository key
at ingest, replacing the vendor's signature, and serves the re-signed
copy. The original is kept, so packages can be re-signed from it when the
key changes. A synced rpm is checked against its upstream checksum before
it is re-signed.

Re-signing uses `rpmsign`, `gpg` and `gpg-agent`, which is why rpm support
needs them installed. garner does this itself for each package; you
never run `rpmsign` by hand.

### Key rotation

Several keys can be configured at once. All are published; the first one
signs. Rotating is four configuration changes, each applied with a reload:

1. Add the new key second. Both keys are published, the old one still
   signs.
2. Wait until every host has fetched the new keyring: apt hosts the
   `.sources` file, dnf hosts `key.asc`.
3. Move the new key first. It now signs, and garner re-signs every rpm
   package with it.
4. Remove the old key.

Only the first key signs, so from step 3 on every signature is made with
the new key, and a host that has not fetched the new keyring by then
fails to verify the repository. Step 2 is what prevents that: give it as
long as your slowest hosts need. The
[key rotation guide](../guides/key-rotation.md) has the commands.

Adding a new signing subkey to the first key, as when the current one is
about to expire, also re-signs every rpm.

## Why it works this way

- **One key to trust.** A host that adds garner's repository trusts
  garner's key and nothing else. Adding a project to the repository does
  not change what hosts trust.
- **garner verifies, clients trust garner.** Checking a project's release
  is done once, by garner, with the project's checksums or digests. The
  result is expressed in garner's signature.
- **Keep the original.** Because the vendor's file is kept, re-signing
  after a key change is repeatable and needs nothing from upstream.

## Limits

- **Images are not signed** by garner. cosign signatures pushed or synced
  as tags (`sha256-<digest>.sig`) are stored and served like any other
  tag; garner does not check them.
- The key must be a file garner can read: there is no HSM or KMS support,
  and rpm re-signing needs the private key in a form `gpg` can import.
- garner does not yet check upstream signatures (OpenPGP, Sigstore) of
  synced releases, only checksums. That is planned.

## See also

- [Key rotation guide](../guides/key-rotation.md)
- [Formats: deb](formats/deb.md), [rpm](formats/rpm.md)
- [Monitoring](../guides/monitoring.md): `garner_signing_key_expiry_days`
  and `garner_signing_resign_pending`
- Configuration: [`signing`](../reference/configuration.md#signing)
