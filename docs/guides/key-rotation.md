# Rotating the signing key

This guide replaces the repository's signing key without breaking any
host. Read [signing](../concepts/signing.md) first for what the key
signs. The whole rotation is four configuration changes; between the
second and the third, every host has to fetch the new keys, which is the
step that takes time.

Only the first configured key signs, so hosts must have the new key
**before** it is promoted. A host without it then fails `apt update` or
`dnf makecache` until it gets it.

## 1. Create the new key

garner accepts any OpenPGP key whose private part is an armored file.
RSA 4096 works with every client garner supports. With `gpg`, in a
throwaway keyring:

```sh
export GNUPGHOME=$(mktemp -d)
gpg --batch --passphrase '' --quick-gen-key \
    'garner repository 2027 <repo@example.com>' rsa4096 sign never
sudo install -d -m 0750 -o root -g garner /etc/garner/keys
sudo install -m 0640 -o root -g garner /dev/null /etc/garner/keys/2027.asc
gpg --armor --export-secret-keys | sudo tee /etc/garner/keys/2027.asc >/dev/null
gpgconf --kill gpg-agent
rm -rf "$GNUPGHOME"
```

This makes a key without passphrase and without expiry, as `garner init`
does. For a passphrase, protect the key with one and add
`passphrase_file`.

## 2. Publish it next to the old key

Add the new key **second**, after the current one.

If `garner init` generated the current key, `signing.keys` is empty and
garner falls back to that key. Once the list has entries, garner uses only
the listed keys, so list the current key first, at
`<storage.path>/signing/key.asc`:

```yaml
signing:
  keys:
    - key_file: /var/lib/garner/signing/key.asc   # current, still signs
    - key_file: /etc/garner/keys/2027.asc         # new
```

```sh
garner config validate /etc/garner/garner.yaml
sudo systemctl reload garner
```

garner publishes both keys in `/key.asc`, `/key.gpg` and the apt client
files. Nothing changes for hosts yet.

## 3. Give every host the new keys

Hosts do not fetch keys on their own. Update each one, through your
configuration management or by hand:

- **apt hosts** carry the keys in their `.sources` file. Download it
  again:

  ```sh
  sudo curl -fsSLo /etc/apt/sources.list.d/garner.sources https://pkg.example.com/garner.sources
  ```

  (or `/clients/<suite>/garner.sources` for another suite).

- **dnf hosts** import keys into rpm's keyring. Import the new one:

  ```sh
  sudo rpm --import https://pkg.example.com/key.asc
  ```

Wait until every host is done, including ones that are switched off or
updated only rarely.

## 4. Promote the new key

Move the new key first:

```yaml
signing:
  keys:
    - key_file: /etc/garner/keys/2027.asc         # signs from now on
    - key_file: /var/lib/garner/signing/key.asc
```

```sh
sudo systemctl reload garner
```

garner signs the next snapshot with the new key and re-signs every rpm
package with it.

Check that a host still updates:

```sh
sudo apt update      # or: sudo dnf makecache
```

## 5. Remove the old key

Once you are sure no host needs the old key, which is to say no host
lacks the new one, remove it from `signing.keys` and reload. Keep a copy
of the old private key with your backups until you are certain you will
not need to go back.

Hosts keep the old key in their keyring until you remove it: apt hosts
when they download the `.sources` file again, dnf hosts with
`rpm -e gpg-pubkey-<id>`.

## Renewing an expiring key instead

If the key has an expiry date, you can extend it rather than replace it:
change the expiry with `gpg --quick-set-expire`, export the private key
again over the configured file, and reload. Hosts need the updated public
key, as in step 3. `garner_signing_key_expiry_days` shows the days left.

Adding a new signing subkey to the first key also makes garner re-sign
every rpm with it.

## See also

- [Signing](../concepts/signing.md)
- [Backup and restore](backup-restore.md): back up the key files
- Configuration: [`signing`](../reference/configuration.md#signing)
