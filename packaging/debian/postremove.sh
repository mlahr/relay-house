#!/bin/sh
set -e

if command -v systemctl >/dev/null 2>&1; then
	systemctl daemon-reload >/dev/null 2>&1 || true
fi

if [ "$1" = "purge" ]; then
	if command -v update-rc.d >/dev/null 2>&1; then
		update-rc.d email-endpoint remove >/dev/null 2>&1 || true
	fi
	rm -rf /var/lib/email-endpoint /run/email-endpoint
fi

exit 0
