#!/bin/sh
set -e

if ! getent group email-endpoint >/dev/null; then
	addgroup --system email-endpoint >/dev/null
fi

if ! getent passwd email-endpoint >/dev/null; then
	adduser --system --home /var/lib/email-endpoint --no-create-home --ingroup email-endpoint --disabled-login --shell /usr/sbin/nologin email-endpoint >/dev/null
fi

install -d -o email-endpoint -g email-endpoint -m 0750 /var/lib/email-endpoint
install -d -o email-endpoint -g email-endpoint -m 0755 /run/email-endpoint
chown root:email-endpoint /etc/email-endpoint
chmod 0750 /etc/email-endpoint
chown root:email-endpoint /etc/email-endpoint/config.yaml
chmod 0640 /etc/email-endpoint/config.yaml

if command -v systemctl >/dev/null 2>&1; then
	systemctl daemon-reload || true
	systemctl enable email-endpoint.service >/dev/null 2>&1 || true
fi

if command -v update-rc.d >/dev/null 2>&1; then
	update-rc.d email-endpoint defaults >/dev/null
fi

exit 0
