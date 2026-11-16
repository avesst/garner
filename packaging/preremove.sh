#!/bin/sh
# deb prerm ("remove", "upgrade", ...) and rpm %preun (0 on erase, 1 on
# upgrade). Removal stops the service; rpm has no purge, so it also
# disables it there.
set -e
[ -d /run/systemd/system ] || exit 0
case "$1" in
remove)
	if command -v deb-systemd-invoke >/dev/null 2>&1; then
		deb-systemd-invoke stop garner.service >/dev/null || true
	else
		systemctl stop garner.service >/dev/null || true
	fi
	;;
0) systemctl --no-reload disable --now garner.service >/dev/null 2>&1 || true ;;
esac
