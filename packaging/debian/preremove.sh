#!/bin/sh
set -e

if [ "$1" = "remove" ] || [ "$1" = "deconfigure" ]; then
	if command -v systemctl >/dev/null 2>&1; then
		systemctl stop email-endpoint.service >/dev/null 2>&1 || true
		systemctl disable email-endpoint.service >/dev/null 2>&1 || true
	fi
	if [ -x /etc/init.d/email-endpoint ]; then
		/etc/init.d/email-endpoint stop >/dev/null 2>&1 || true
	fi
fi

exit 0
