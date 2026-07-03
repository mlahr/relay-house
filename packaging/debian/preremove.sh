#!/bin/sh
set -e

if [ "$1" = "remove" ] || [ "$1" = "deconfigure" ]; then
	if command -v systemctl >/dev/null 2>&1; then
		systemctl stop relay-house.service >/dev/null 2>&1 || true
		systemctl disable relay-house.service >/dev/null 2>&1 || true
	fi
	if [ -x /etc/init.d/relay-house ]; then
		/etc/init.d/relay-house stop >/dev/null 2>&1 || true
	fi
fi

exit 0
