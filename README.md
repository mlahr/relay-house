# RelayHouse

Self-hosted email relay for browser-based website forms.

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
cp config.example.yaml config.yaml
# edit config.yaml
go run ./cmd/server --config config.yaml
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

RelayHouse can be configured with YAML, environment variables, or both.

Start from the example YAML file:

```sh
cp config.example.yaml config.yaml
$EDITOR config.yaml
relay-house -config config.yaml
```

For Docker/local environment variable mode, start from `.env.example`:

```sh
cp .env.example .env
$EDITOR .env
```

Configuration precedence is:

```txt
compiled defaults < YAML config from -config < environment variables
```

That means environment variables override matching YAML values.

`PROJECT_KEY` is a public identifier used by your website JavaScript. It is not a secret.

`RECIPIENTS` and `MAIL_FROM` are server-controlled. The browser cannot choose arbitrary recipients.

`TURNSTILE_SECRET` is optional. If set, requests must include `turnstileToken`.

`PROJECT_KEY` and `IP_HASH_SECRET` are generated if omitted. Set both explicitly in production so the browser project key and rate-limit hashing remain stable across restarts.

### YAML Reference

| YAML key | Environment variable | Default | Notes |
| --- | --- | --- | --- |
| `http.address` | `ADDR` | `:8080` | HTTP listen address. |
| `database.path` | `DATABASE_PATH` | `relay-house.db` | SQLite database path. |
| `project.key` | `PROJECT_KEY` | generated | Public project key expected in request JSON. Set explicitly in production. |
| `project.name` | `PROJECT_NAME` | `default` | Stored project display name. |
| `project.allowed_origins` | `ALLOWED_ORIGINS` | none | Required. YAML list or comma-separated env var of exact browser origins. |
| `project.recipients` | `RECIPIENTS` | none | Required. YAML list or comma-separated env var of destination addresses. |
| `mail.from` | `MAIL_FROM` | none | Required. Server-controlled sender address. |
| `mail.delivery_provider` | `DELIVERY_PROVIDER` | `smtp` | Supported values: `smtp`, `mailtrap`. |
| `mail.smtp.host` | `SMTP_HOST` | none | Required when `mail.delivery_provider` is `smtp`. |
| `mail.smtp.port` | `SMTP_PORT` | `587` | SMTP port. |
| `mail.smtp.username` | `SMTP_USERNAME` | none | Required when `mail.delivery_provider` is `smtp`. |
| `mail.smtp.password` | `SMTP_PASSWORD` | none | Required when `mail.delivery_provider` is `smtp`. |
| `mail.smtp.insecure_plain_auth` | `SMTP_INSECURE_PLAIN_AUTH` | `false` | Use only for SMTP servers requiring plaintext auth without TLS. |
| `mail.mailtrap.api_url` | `MAILTRAP_API_URL` | `https://send.api.mailtrap.io/api/send` | Required when `mail.delivery_provider` is `mailtrap`. |
| `mail.mailtrap.api_token` | `MAILTRAP_API_TOKEN` | none | Required when `mail.delivery_provider` is `mailtrap`. |
| `mail.mailtrap.bcc` | `MAILTRAP_BCC` | empty | Optional BCC recipients for Mailtrap API delivery. |
| `turnstile.secret` | `TURNSTILE_SECRET` | empty | Optional Cloudflare Turnstile secret. |
| `rate_limit.per_minute` | `RATE_LIMIT_PER_MINUTE` | `5` | Limit per project and hashed client IP. |
| `rate_limit.per_day` | `RATE_LIMIT_PER_DAY` | `100` | Limit per project and hashed client IP. |
| `worker.max_retries` | `MAX_RETRIES` | `5` | Maximum delivery attempts before a job is marked dead. |
| `worker.interval_seconds` | `WORKER_INTERVAL_SECONDS` | `5` | Worker polling interval. |
| `retention.days` | `RETENTION_DAYS` | `365` | Deletes sent and dead submissions older than this. Set `0` to disable cleanup. |
| `security.ip_hash_secret` | `IP_HASH_SECRET` | generated | Secret used to hash client IPs. Set explicitly in production. |

### Minimal SMTP YAML

```yaml
project:
  key: replace-with-public-project-key
  allowed_origins:
    - https://example.com
  recipients:
    - Website Owner <you@example.com>

mail:
  from: Website Contact <contact@example.com>
  delivery_provider: smtp
  smtp:
    host: smtp.example.com
    username: smtp-user
    password: smtp-password

security:
  ip_hash_secret: replace-with-random-secret
```

### Minimal Mailtrap YAML

```yaml
project:
  key: replace-with-public-project-key
  allowed_origins:
    - https://example.com
  recipients:
    - Website Owner <you@example.com>

mail:
  from: Website Contact <contact@example.com>
  delivery_provider: mailtrap
  mailtrap:
    api_token: your-token

security:
  ip_hash_secret: replace-with-random-secret
```

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

## Inspect the Database

Print recent database-derived events and exit:

```sh
relay-house events -config config.yaml
```

On Debian/package installs, `relay-house events` automatically uses `/etc/relay-house/config.yaml` when that file exists. If you set `RELAY_HOUSE_CONFIG`, the command uses that path when `-config` is not provided.

Useful options:

```sh
relay-house events -config config.yaml -limit 50
relay-house events -database /var/lib/relay-house/relay-house.db --json
```

The `events` command opens SQLite read-only and does not print stored contact form names, email addresses, subjects, or message bodies.

## Debian Install

Install the latest GitHub release package:

```sh
curl -fsSL https://github.com/mlahr/relay-house/releases/latest/download/install.sh | bash
```

Or install from a checked-out repo after building a release `.deb`.

Package paths:

```txt
/usr/bin/relay-house
/etc/relay-house/config.yaml
/etc/default/relay-house
/var/lib/relay-house/relay-house.db
```

The package enables the service but keeps it disabled until configured. Edit:

```sh
sudoedit /etc/relay-house/config.yaml
sudoedit /etc/default/relay-house
```

Set:

```sh
RELAY_HOUSE_ENABLED=true
```

Then start it:

```sh
sudo systemctl restart relay-house
sudo systemctl status relay-house
journalctl -u relay-house -f
```

## Release

Create a tag to publish GoReleaser artifacts:

```sh
git tag v0.1.0
git push origin v0.1.0
```

The release workflow publishes Linux `amd64` tarballs, `.deb` packages, and `checksums.txt`.
