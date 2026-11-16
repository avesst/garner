#!/bin/sh
# deb postrm ("remove", "purge", "upgrade", ...) and rpm %postun (0 on
# erase, 1 on upgrade). Purge drops the enable link that systemctl
# enable left; the user and /var/lib/garner are kept, as they hold data.
set -e
if [ "$1" = purge ]; then
	rm -f /etc/systemd/system/multi-user.target.wants/garner.service
fi
if [ -d /run/systemd/system ]; then
	systemctl --system daemon-reload >/dev/null || true
fi
