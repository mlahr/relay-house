# email-endpoint

Self-hosted email endpoint for browser-based website forms.

## What It Does

- Accepts `POST /v1/send` from configured public website origins.
- Stores submissions, delivery jobs, attempts, retries, and status in SQLite.
- Sends mail through SMTP.
- Retries failed deliveries with exponential backoff.
- Keeps full submissions in SQLite for a configurable retention window.
- Uses hashed client IPs for rate limiting.
- Optionally verifies Cloudflare Turnstile tokens.

## Run

```sh
cp .env.example .env
# edit .env
docker compose up --build
```

For local development:

```sh
go run ./cmd/server
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
