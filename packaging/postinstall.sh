#!/bin/sh
# deb postinst ("configure" [old-version]) and rpm %post (1 on install,
# 2 on upgrade). The service is not enabled: it needs a configuration
# and garner init first. An upgrade restarts it if it runs.
set -e
case "$1" in
configure) upgrade=${2:+yes} ;;
1) upgrade= ;;
2) upgrade=yes ;;
*) exit 0 ;;
esac

# The user and the state directory, before anything runs as garner.
if command -v systemd-sysusers >/dev/null 2>&1; then
	systemd-sysusers garner.conf
elif ! getent passwd garner >/dev/null; then
	useradd --system --user-group --home-dir /var/lib/garner --no-create-home \
		--shell /usr/sbin/nologin --comment "garner package repository" garner
fi
if [ -L /var/lib/garner ] && [ -d /var/lib/private/garner ]; then
	# Left by a version with DynamicUser=yes: a link to
	# /var/lib/private/garner, owned by the old dynamic UID. Move it back
	# and give it to garner; systemd would move it but not chown it.
	rm /var/lib/garner
	mv /var/lib/private/garner /var/lib/garner
	chown -R garner:garner /var/lib/garner
	chmod 0750 /var/lib/garner
elif command -v systemd-tmpfiles >/dev/null 2>&1; then
	systemd-tmpfiles --create garner.conf
else
	install -d -m 0750 -o garner -g garner /var/lib/garner
fi

if [ -d /run/systemd/system ]; then
	systemctl --system daemon-reload >/dev/null || true
	if [ -n "$upgrade" ]; then
		# deb-systemd-invoke honours policy-rc.d, so chroots and image
		# builds do not start services.
		if command -v deb-systemd-invoke >/dev/null 2>&1; then
			deb-systemd-invoke try-restart garner.service >/dev/null || true
		else
			systemctl try-restart garner.service >/dev/null || true
		fi
	fi
fi
