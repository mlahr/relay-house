#!/bin/sh
set -e

if ! getent group relay-house >/dev/null; then
	addgroup --system relay-house >/dev/null
fi

if ! getent passwd relay-house >/dev/null; then
	adduser --system --home /var/lib/relay-house --no-create-home --ingroup relay-house --disabled-login --shell /usr/sbin/nologin relay-house >/dev/null
fi

install -d -o relay-house -g relay-house -m 0750 /var/lib/relay-house
install -d -o relay-house -g relay-house -m 0755 /run/relay-house
chown root:relay-house /etc/relay-house
chmod 0750 /etc/relay-house
chown root:relay-house /etc/relay-house/config.yaml
chmod 0640 /etc/relay-house/config.yaml

if command -v systemctl >/dev/null 2>&1; then
	systemctl daemon-reload || true
	systemctl enable relay-house.service >/dev/null 2>&1 || true
fi

if command -v update-rc.d >/dev/null 2>&1; then
	update-rc.d relay-house defaults >/dev/null
fi

exit 0
