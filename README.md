# RelayHouse

Self-hosted form relay for browser-based website forms.

## What It Does

- Accepts `POST /v1/send` from configured public website origins.
- Stores submissions, delivery jobs, attempts, retries, and status in SQLite.
- Delivers submissions through SMTP, Mailtrap's transactional REST API, or Telegram.
- Retries failed deliveries with exponential backoff.
- Keeps full submissions in SQLite for a configurable retention window.
- Uses hashed client IPs for rate limiting.
- Optionally verifies Cloudflare Turnstile tokens.

## Run

Docker:

```sh
cp config.example.yaml config.yaml
cp .env.example .env
# edit config.yaml and optional runtime overrides in .env
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

## Health

`GET /healthz` reports whether every configured delivery path is currently
healthy. The endpoint is unauthenticated and returns provider names and types.

A healthy response uses `200 OK`:

```json
{
  "status": "ok",
  "database": {
    "status": "ok"
  },
  "providers": [
    {
      "name": "smtp-main",
      "type": "smtp",
      "status": "ok",
      "live_check": "ok",
      "live_checked_at": "2026-07-20T10:00:00Z",
      "last_attempt": "succeeded",
      "last_attempt_at": "2026-07-20T09:58:00Z"
    }
  ],
  "checked_at": "2026-07-20T10:00:12Z"
}
```

The endpoint returns `503 Service Unavailable` when the database or any
configured provider is unhealthy. Provider failures contain stable
`reason_codes`; raw external errors and credentials are never returned.

Health is evaluated as follows:

- SQLite must accept a ping, and delivery-attempt history must be readable.
- SMTP providers must accept a connection and authentication. The probe does
  not issue SMTP `MAIL`, `RCPT`, or `DATA` commands.
- Telegram providers must accept a `getMe` authentication request.
- Mailtrap providers are not called by health checks; their status is derived
  only from recorded delivery attempts.
- If a provider has recorded attempts, its most recent attempt must have
  succeeded. A provider with no recorded attempt passes this condition.

SMTP and Telegram probe results are cached for 60 seconds. Delivery history is
read on every health request. `succeeded` means that RelayHouse's configured
provider accepted the attempt; it does not confirm delivery to a recipient's
inbox.

Possible provider reason codes are `connectivity_failed`,
`authentication_failed`, `probe_timeout`, `unexpected_response`,
`latest_attempt_failed`, and `history_unavailable`. Database reason codes are
`database_unavailable` and `history_unavailable`.

## Configuration

RelayHouse provider instances and project definitions are configured with YAML.
Runtime settings can be overridden with environment variables. Provider
credentials and destinations are YAML-only.

Start from the example YAML file:

```sh
cp config.example.yaml config.yaml
$EDITOR config.yaml
relay-house -config config.yaml
```

For Docker/local runtime overrides, start from `.env.example`:

```sh
cp .env.example .env
$EDITOR .env
```

Runtime configuration precedence is:

```txt
compiled defaults < YAML config from -config < environment variables
```

That means environment variables override matching runtime YAML values. Provider settings are not overridden by environment variables.

Each `projects[].key` is a public identifier used by your website JavaScript. It is not a secret.

Each project selects one provider instance with `projects[].provider`. Provider instances own their destinations: SMTP and Mailtrap providers own `from` and `recipients`, and Telegram providers own `chat_ids`.

`TURNSTILE_SECRET` is optional. If set, requests must include `turnstileToken`.

Set `projects[].key` and `security.ip_hash_secret` explicitly in production so the browser project key and rate-limit hashing remain stable across restarts.

### YAML Reference

| YAML key | Environment variable | Default | Notes |
| --- | --- | --- | --- |
| `http.address` | `ADDR` | `:8080` | HTTP listen address. |
| `database.path` | `DATABASE_PATH` | `relay-house.db` | SQLite database path. |
| `providers[].name` | none | none | Required. Unique provider instance name. |
| `providers[].type` | none | none | Required. Supported values: `smtp`, `mailtrap`, `telegram`. |
| `providers[].from` | none | none | Required for `smtp` and `mailtrap`. Server-controlled sender address. |
| `providers[].recipients` | none | none | Required for `smtp` and `mailtrap`. Destination email addresses. |
| `providers[].host` | none | none | Required for `smtp`. SMTP host. |
| `providers[].port` | none | `587` | SMTP port. |
| `providers[].username` | none | none | Required for `smtp`. |
| `providers[].password` | none | none | Required for `smtp`. |
| `providers[].insecure_plain_auth` | none | `false` | SMTP only. Use only for SMTP servers requiring plaintext auth without TLS. |
| `providers[].api_url` | none | `https://send.api.mailtrap.io/api/send` | Mailtrap API URL. |
| `providers[].api_token` | none | none | Required for `mailtrap`. |
| `providers[].bcc` | none | empty | Optional Mailtrap BCC recipients. |
| `providers[].bot_token` | none | none | Required for `telegram`. |
| `providers[].chat_ids` | none | empty | Required for `telegram`. |
| `projects[].key` | none | none | Required. Public project key expected in request JSON. |
| `projects[].name` | none | project key | Stored project display name. |
| `projects[].allowed_origins` | none | none | Required. Exact browser origins for this project. |
| `projects[].provider` | none | none | Required. References `providers[].name`. |
| `turnstile.secret` | `TURNSTILE_SECRET` | empty | Optional Cloudflare Turnstile secret. |
| `rate_limit.per_minute` | `RATE_LIMIT_PER_MINUTE` | `5` | Limit per project and hashed client IP. |
| `rate_limit.per_day` | `RATE_LIMIT_PER_DAY` | `100` | Limit per project and hashed client IP. |
| `worker.max_retries` | `MAX_RETRIES` | `5` | Maximum delivery attempts before a job is marked dead. |
| `worker.interval_seconds` | `WORKER_INTERVAL_SECONDS` | `5` | Worker polling interval. |
| `retention.days` | `RETENTION_DAYS` | `365` | Deletes sent and dead submissions older than this. Set `0` to disable cleanup. |
| `security.ip_hash_secret` | `IP_HASH_SECRET` | generated | Secret used to hash client IPs. Set explicitly in production. |

### Minimal SMTP YAML

```yaml
providers:
  - name: smtp-main
    type: smtp
    from: Website Contact <contact@example.com>
    recipients:
      - Website Owner <you@example.com>
    host: smtp.example.com
    username: smtp-user
    password: smtp-password

projects:
  - key: replace-with-public-project-key
    allowed_origins:
      - https://example.com
    provider: smtp-main

security:
  ip_hash_secret: replace-with-random-secret
```

### Minimal Mailtrap YAML

```yaml
providers:
  - name: mailtrap-main
    type: mailtrap
    from: Website Contact <contact@example.com>
    recipients:
      - Website Owner <you@example.com>
    api_token: your-token

projects:
  - key: replace-with-public-project-key
    allowed_origins:
      - https://example.com
    provider: mailtrap-main

security:
  ip_hash_secret: replace-with-random-secret
```

### Minimal Telegram YAML

```yaml
providers:
  - name: telegram-main
    type: telegram
    bot_token: your-bot-token
    chat_ids:
      - 123456789

projects:
  - key: replace-with-public-project-key
    allowed_origins:
      - https://example.com
    provider: telegram-main

security:
  ip_hash_secret: replace-with-random-secret
```

## Delivery Providers

SMTP providers use `type: smtp` and require `from`, `recipients`, `host`, `username`, and `password`.

Mailtrap providers use `type: mailtrap` and require `from`, `recipients`, and `api_token`.

For Mailtrap Sandbox, set `api_url` to the sandbox send endpoint for your inbox.

Telegram providers use `type: telegram` and require `bot_token` and `chat_ids`.

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
