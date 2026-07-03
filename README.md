# email-endpoint

Self-hosted email endpoint for browser-based website forms.

## What It Does

- Accepts `POST /v1/send` from configured public website origins.
- Stores submissions, delivery jobs, attempts, retries, and status in SQLite.
- Sends mail through SMTP or Mailtrap's transactional REST API.
- Retries failed deliveries with exponential backoff.
- Keeps full submissions in SQLite for a configurable retention window.
- Uses hashed client IPs for rate limiting.
- Optionally verifies Cloudflare Turnstile tokens.

## Run

Docker/local env mode:

```sh
cp .env.example .env
# edit .env
docker compose up --build
```

For local development:

```sh
go run ./cmd/server
```

YAML config mode:

```sh
go run ./cmd/server --config packaging/debian/email-endpoint.config.yaml
```

The Go binary loads config in this order:

```txt
compiled defaults < YAML config from --config < environment variables
```

## Request

```http
POST /v1/send
Origin: https://example.com
Content-Type: application/json
```

```json
{
  "project": "replace-with-public-project-key",
  "name": "Jane Doe",
  "email": "jane@example.net",
  "subject": "Website inquiry",
  "message": "Hello",
  "turnstileToken": "optional-token"
}
```

Success returns `202 Accepted`:

```json
{
  "id": "submission-uuid",
  "status": "queued"
}
```

## Configuration

See `.env.example`.

`PROJECT_KEY` is a public identifier used by your website JavaScript. It is not a secret.

`RECIPIENTS` and `MAIL_FROM` are server-controlled. The browser cannot choose arbitrary recipients.

`TURNSTILE_SECRET` is optional. If set, requests must include `turnstileToken`.

## Delivery Providers

SMTP:

```env
DELIVERY_PROVIDER=smtp
SMTP_HOST=smtp.example.com
SMTP_PORT=587
SMTP_USERNAME=smtp-user
SMTP_PASSWORD=smtp-password
```

Mailtrap REST API:

```env
DELIVERY_PROVIDER=mailtrap
MAILTRAP_API_TOKEN=your-token
MAILTRAP_API_URL=https://send.api.mailtrap.io/api/send
MAILTRAP_BCC=optional@example.com
```

For Mailtrap Sandbox, set `MAILTRAP_API_URL` to the sandbox send endpoint for your inbox.

## Debian Install

Install the latest GitHub release package:

```sh
curl -fsSL https://github.com/mlahr/email-endpoint/releases/latest/download/install.sh | bash
```

Or install from a checked-out repo after building a release `.deb`.

Package paths:

```txt
/usr/bin/email-endpoint
/etc/email-endpoint/config.yaml
/etc/default/email-endpoint
/var/lib/email-endpoint/email-endpoint.db
```

The package enables the service but keeps it disabled until configured. Edit:

```sh
sudoedit /etc/email-endpoint/config.yaml
sudoedit /etc/default/email-endpoint
```

Set:

```sh
EMAIL_ENDPOINT_ENABLED=true
```

Then start it:

```sh
sudo systemctl restart email-endpoint
sudo systemctl status email-endpoint
journalctl -u email-endpoint -f
```

## Release

Create a tag to publish GoReleaser artifacts:

```sh
git tag v0.1.0
git push origin v0.1.0
```

The release workflow publishes Linux `amd64` tarballs, `.deb` packages, and `checksums.txt`.
