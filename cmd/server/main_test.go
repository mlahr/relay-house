package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestSendMailtrap(t *testing.T) {
	var gotAuth string
	var gotPayload map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotPayload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"success":true,"message_ids":["abc"]}`))
	}))
	defer server.Close()

	app := &App{
		cfg: Config{
			From:             "Website Contact <contact@example.com>",
			Recipients:       []string{"Owner <owner@example.com>"},
			MailtrapAPIURL:   server.URL,
			MailtrapAPIToken: "secret-token",
			MailtrapBCC:      []string{"Audit <audit@example.com>"},
		},
	}

	response, err := app.sendMailtrap(context.Background(), job{
		Name:    "Jane Doe",
		Email:   "jane@example.net",
		Subject: "Subject",
		Message: "Message body",
	})
	if err != nil {
		t.Fatalf("sendMailtrap returned error: %v", err)
	}
	if response != `{"success":true,"message_ids":["abc"]}` {
		t.Fatalf("response = %q", response)
	}
	if gotAuth != "Bearer secret-token" {
		t.Fatalf("Authorization = %q", gotAuth)
	}

	from := gotPayload["from"].(map[string]any)
	if from["email"] != "contact@example.com" || from["name"] != "Website Contact" {
		t.Fatalf("from = %#v", from)
	}
	replyTo := gotPayload["reply_to"].(map[string]any)
	if replyTo["email"] != "jane@example.net" || replyTo["name"] != "Jane Doe" {
		t.Fatalf("reply_to = %#v", replyTo)
	}
	if gotPayload["subject"] != "Subject" || gotPayload["text"] != "Message body" {
		t.Fatalf("payload = %#v", gotPayload)
	}
}

func TestLoadConfigMailtrapDoesNotRequireSMTP(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DELIVERY_PROVIDER", "mailtrap")
	t.Setenv("PROJECT_KEY", "test-project")
	t.Setenv("ALLOWED_ORIGINS", "https://example.com")
	t.Setenv("RECIPIENTS", "owner@example.com")
	t.Setenv("MAIL_FROM", "Website Contact <contact@example.com>")
	t.Setenv("MAILTRAP_API_TOKEN", "secret-token")
	t.Setenv("IP_HASH_SECRET", "secret")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig returned error: %v", err)
	}
	if cfg.DeliveryProvider != "mailtrap" {
		t.Fatalf("DeliveryProvider = %q", cfg.DeliveryProvider)
	}
}

func TestLoadConfigFromYAML(t *testing.T) {
	clearConfigEnv(t)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	writeTestConfig(t, configPath, `
http:
  address: 127.0.0.1:18080
database:
  path: /var/lib/relay-house/relay-house.db
project:
  key: yaml-project
  name: yaml-site
  allowed_origins:
    - https://example.com
  recipients:
    - Owner <owner@example.com>
mail:
  from: Website Contact <contact@example.com>
  delivery_provider: mailtrap
  mailtrap:
    api_url: https://send.api.mailtrap.io/api/send
    api_token: yaml-token
    bcc:
      - Audit <audit@example.com>
rate_limit:
  per_minute: 9
  per_day: 90
worker:
  max_retries: 7
  interval_seconds: 11
retention:
  days: 45
security:
  ip_hash_secret: yaml-secret
`)

	cfg, err := loadConfigFromArgs([]string{"--config", configPath})
	if err != nil {
		t.Fatalf("loadConfigFromArgs returned error: %v", err)
	}
	if cfg.Addr != "127.0.0.1:18080" {
		t.Fatalf("Addr = %q", cfg.Addr)
	}
	if cfg.DatabasePath != "/var/lib/relay-house/relay-house.db" {
		t.Fatalf("DatabasePath = %q", cfg.DatabasePath)
	}
	if cfg.ProjectKey != "yaml-project" || cfg.ProjectName != "yaml-site" {
		t.Fatalf("project = %q/%q", cfg.ProjectKey, cfg.ProjectName)
	}
	if cfg.DeliveryProvider != "mailtrap" || cfg.MailtrapAPIToken != "yaml-token" {
		t.Fatalf("mailtrap config = %#v", cfg)
	}
	if cfg.RateMinute != 9 || cfg.RateDay != 90 || cfg.MaxRetries != 7 || cfg.WorkerInterval.Seconds() != 11 || cfg.RetentionDays != 45 {
		t.Fatalf("numeric config = %#v", cfg)
	}
}

func TestEnvOverridesYAMLConfig(t *testing.T) {
	clearConfigEnv(t)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	writeTestConfig(t, configPath, `
http:
  address: 127.0.0.1:18080
database:
  path: yaml.db
project:
  key: yaml-project
  allowed_origins:
    - https://example.com
  recipients:
    - owner@example.com
mail:
  from: Website Contact <contact@example.com>
  delivery_provider: mailtrap
  mailtrap:
    api_token: yaml-token
security:
  ip_hash_secret: yaml-secret
`)
	t.Setenv("ADDR", ":19090")
	t.Setenv("DELIVERY_PROVIDER", "smtp")
	t.Setenv("SMTP_HOST", "smtp.example.com")
	t.Setenv("SMTP_USERNAME", "smtp-user")
	t.Setenv("SMTP_PASSWORD", "smtp-password")

	cfg, err := loadConfigFromArgs([]string{"--config", configPath})
	if err != nil {
		t.Fatalf("loadConfigFromArgs returned error: %v", err)
	}
	if cfg.Addr != ":19090" {
		t.Fatalf("Addr = %q", cfg.Addr)
	}
	if cfg.DeliveryProvider != "smtp" || cfg.SMTPHost != "smtp.example.com" {
		t.Fatalf("provider config = %#v", cfg)
	}
}

func TestEnvelopeAddressesStripsDisplayNames(t *testing.T) {
	got, err := envelopeAddresses([]string{
		"Owner <owner@example.com>",
		"audit@example.com",
	})
	if err != nil {
		t.Fatalf("envelopeAddresses returned error: %v", err)
	}
	want := []string{"owner@example.com", "audit@example.com"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func writeTestConfig(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func clearConfigEnv(t *testing.T) {
	t.Helper()
	keys := []string{
		"ADDR",
		"DATABASE_PATH",
		"DELIVERY_PROVIDER",
		"PROJECT_KEY",
		"PROJECT_NAME",
		"ALLOWED_ORIGINS",
		"RECIPIENTS",
		"MAIL_FROM",
		"SMTP_HOST",
		"SMTP_PORT",
		"SMTP_USERNAME",
		"SMTP_PASSWORD",
		"SMTP_INSECURE_PLAIN_AUTH",
		"MAILTRAP_API_URL",
		"MAILTRAP_API_TOKEN",
		"MAILTRAP_BCC",
		"TURNSTILE_SECRET",
		"RATE_LIMIT_PER_MINUTE",
		"RATE_LIMIT_PER_DAY",
		"MAX_RETRIES",
		"WORKER_INTERVAL_SECONDS",
		"RETENTION_DAYS",
		"IP_HASH_SECRET",
	}
	type savedEnv struct {
		value string
		ok    bool
	}
	saved := make(map[string]savedEnv, len(keys))
	for _, key := range keys {
		value, ok := os.LookupEnv(key)
		saved[key] = savedEnv{value: value, ok: ok}
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("unset %s: %v", key, err)
		}
	}
	t.Cleanup(func() {
		for _, key := range keys {
			item := saved[key]
			if item.ok {
				_ = os.Setenv(key, item.value)
			} else {
				_ = os.Unsetenv(key)
			}
		}
	})
}
