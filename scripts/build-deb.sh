#!/bin/sh
# Build a .deb of garner with dpkg-deb: the static binary, the systemd
# unit and an example configuration. Usage: build-deb.sh [OUTDIR]
# GOARCH selects the architecture (default: the Go host architecture).
set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
out=${1:-$root/bin}
arch=${GOARCH:-$(go env GOARCH)}
case $arch in
amd64 | arm64) ;;
*) echo "unsupported GOARCH $arch" >&2; exit 1 ;;
esac

# Debian versions must start with a digit: v1.2.3 gives 1.2.3, commits
# after it 1.2.3+4.gabc1234, and an untagged tree 0.0~git<date>.<hash>.
desc=$(git -C "$root" describe --tags --always --dirty)
case $desc in
v[0-9]* | [0-9]*)
	version=$(echo "${desc#v}" | sed 's/-\([0-9]*\)-g/+\1.g/; s/-dirty$/+dirty/') ;;
*)
	date=$(git -C "$root" log -1 --format=%cd --date=format:%Y%m%d)
	version="0.0~git$date.$(echo "$desc" | sed 's/-dirty$/+dirty/')" ;;
esac

stage=$(mktemp -d)
chmod 0755 "$stage" # mktemp makes it 0700; it becomes the "./" entry
trap 'rm -rf "$stage"' EXIT
mkdir -p "$stage/DEBIAN" "$stage/etc/garner" "$stage/usr/bin" "$stage/usr/lib/systemd/system" "$stage/usr/share/doc/garner"

CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -C "$root" -trimpath \
	-ldflags "-s -w -X main.version=$version" -o "$stage/usr/bin/garner" ./cmd/garner
# The contrib unit's leading comment describes a manual install; replace it.
{
	cat <<'HEADER'
# garner, installed by the garner package. Before the first start:
# 1. Set server.public_url in /etc/garner/garner.yaml.
# 2. Run init as the service's dynamic user; it prints the admin token:
#      systemd-run --pty --wait -p DynamicUser=yes -p StateDirectory=garner \
#        -p ConfigurationDirectory=garner /usr/bin/garner init
# 3. systemctl enable --now garner
# rpm support needs the recommended packages createrepo-c, rpm and gpg.
HEADER
	sed '1,/^$/d; s|/usr/local/bin/garner|/usr/bin/garner|g' "$root/contrib/garner.service"
} > "$stage/usr/lib/systemd/system/garner.service"
# A conffile: dpkg keeps local edits on upgrade and removes it on purge.
cp "$root/contrib/garner.yaml" "$stage/etc/garner/garner.yaml"
echo /etc/garner/garner.yaml > "$stage/DEBIAN/conffiles"
cp "$root/docs/design.md" "$stage/usr/share/doc/garner/design.md"
gzip -9n "$stage/usr/share/doc/garner/design.md"

cat > "$stage/DEBIAN/control" <<CONTROL
Package: garner
Version: $version
Architecture: $arch
Maintainer: garner maintainers <garner@localhost>
Installed-Size: $(du -sk "$stage/usr" "$stage/etc" | awk '{s += $1} END {print s}')
Recommends: createrepo-c, rpm, gpg, gpgconf
Section: admin
Priority: optional
Homepage: https://github.com/avesst/garner
Description: internal deb and rpm package repository
 garner serves signed apt and dnf repositories from uploads and from
 releases synced from GitHub, GitLab and Forgejo/Gitea. createrepo-c,
 rpm and gpg are needed for rpm support only.
CONTROL

# The service is not enabled or started: it needs a configuration and
# garner init first.
cat > "$stage/DEBIAN/postinst" <<'POSTINST'
#!/bin/sh
set -e
if [ "$1" = configure ] && [ -d /run/systemd/system ]; then
	systemctl daemon-reload || true
fi
POSTINST
cat > "$stage/DEBIAN/postrm" <<'POSTRM'
#!/bin/sh
set -e
if [ -d /run/systemd/system ]; then
	systemctl daemon-reload || true
fi
POSTRM
chmod 0755 "$stage/DEBIAN/postinst" "$stage/DEBIAN/postrm"
# md5sums lists everything except conffiles, which dpkg tracks itself.
(cd "$stage" && find usr -type f -exec md5sum {} + > DEBIAN/md5sums)

mkdir -p "$out"
deb="$out/garner_${version}_${arch}.deb"
dpkg-deb --root-owner-group -Zxz --build "$stage" "$deb" >/dev/null
echo "$deb"
