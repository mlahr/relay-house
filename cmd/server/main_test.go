package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestHasHelpFlag(t *testing.T) {
	for _, args := range [][]string{
		{"-h"},
		{"--help"},
		{"--config", "config.yaml", "--help"},
	} {
		if !hasHelpFlag(args) {
			t.Fatalf("hasHelpFlag(%v) = false, want true", args)
		}
	}
	if hasHelpFlag([]string{"--config", "help.yaml"}) {
		t.Fatal("hasHelpFlag returned true for non-help args")
	}
}

func TestPrintUsage(t *testing.T) {
	var buf bytes.Buffer
	printUsage(&buf)
	output := buf.String()
	for _, want := range []string{
		"relay-house ",
		"Usage:",
		"relay-house [options]",
		"-h, --help",
		"-config, --config PATH",
		"events",
		"Validation-required settings:",
		"MAILTRAP_API_TOKEN",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("usage output missing %q:\n%s", want, output)
		}
	}
}

func TestEventDatabasePathPrecedence(t *testing.T) {
	clearConfigEnv(t)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	writeTestConfig(t, configPath, `
database:
  path: yaml.db
`)

	got, err := eventDatabasePath("", "")
	if err != nil {
		t.Fatalf("eventDatabasePath default returned error: %v", err)
	}
	if got != "relay-house.db" {
		t.Fatalf("default database path = %q", got)
	}

	got, err = eventDatabasePath(configPath, "")
	if err != nil {
		t.Fatalf("eventDatabasePath YAML returned error: %v", err)
	}
	if got != "yaml.db" {
		t.Fatalf("YAML database path = %q", got)
	}

	t.Setenv("RELAY_HOUSE_CONFIG", configPath)
	got, err = eventDatabasePath("", "")
	if err != nil {
		t.Fatalf("eventDatabasePath RELAY_HOUSE_CONFIG returned error: %v", err)
	}
	if got != "yaml.db" {
		t.Fatalf("RELAY_HOUSE_CONFIG database path = %q", got)
	}

	t.Setenv("DATABASE_PATH", "env.db")
	got, err = eventDatabasePath(configPath, "")
	if err != nil {
		t.Fatalf("eventDatabasePath env returned error: %v", err)
	}
	if got != "env.db" {
		t.Fatalf("env database path = %q", got)
	}

	got, err = eventDatabasePath(configPath, "flag.db")
	if err != nil {
		t.Fatalf("eventDatabasePath override returned error: %v", err)
	}
	if got != "flag.db" {
		t.Fatalf("override database path = %q", got)
	}
}

func TestParseEventCommandOptions(t *testing.T) {
	clearConfigEnv(t)
	opts, err := parseEventCommandOptions([]string{"-database", "events.db", "-limit", "7", "--json"})
	if err != nil {
		t.Fatalf("parseEventCommandOptions returned error: %v", err)
	}
	if opts.DatabasePath != "events.db" || opts.Limit != 7 || !opts.JSON {
		t.Fatalf("opts = %#v", opts)
	}

	if _, err := parseEventCommandOptions([]string{"-limit", "0"}); err == nil {
		t.Fatal("parseEventCommandOptions accepted limit 0")
	}
	if _, err := parseEventCommandOptions([]string{"-limit", "501"}); err == nil {
		t.Fatal("parseEventCommandOptions accepted limit 501")
	}
}

func TestLoadEventsReportsMissingDatabasePath(t *testing.T) {
	_, err := loadEvents(eventCommandOptions{DatabasePath: filepath.Join(t.TempDir(), "missing.db"), Limit: 1})
	if err == nil {
		t.Fatal("loadEvents returned nil error for missing database")
	}
	if !strings.Contains(err.Error(), "does not exist") || !strings.Contains(err.Error(), "pass -config or -database") {
		t.Fatalf("missing database error = %v", err)
	}
}

func TestQueryEventsAndRenderersOmitMessageContent(t *testing.T) {
	db := openSeededEventDB(t)
	defer db.Close()

	events, err := queryEvents(context.Background(), db, 4)
	if err != nil {
		t.Fatalf("queryEvents returned error: %v", err)
	}
	if len(events) != 4 {
		t.Fatalf("len(events) = %d, want 4", len(events))
	}
	wantEvents := []string{
		"delivery.job.current",
		"delivery.attempt.failed",
		"delivery.attempt.sent",
		"submission.accepted",
	}
	for i, want := range wantEvents {
		if events[i].Event != want {
			t.Fatalf("events[%d].Event = %q, want %q: %#v", i, events[i].Event, want, events)
		}
	}
	if events[0].Status != "retry" || events[0].Attempt != 2 || events[0].Summary != "smtp temporary failure" {
		t.Fatalf("current job event = %#v", events[0])
	}

	var table bytes.Buffer
	if err := writeEventsTable(&table, events); err != nil {
		t.Fatalf("writeEventsTable returned error: %v", err)
	}
	assertNoSensitiveEventOutput(t, table.String())
	for _, want := range []string{"time", "delivery.attempt.failed", "submission.accepted", "smtp temporary failure"} {
		if !strings.Contains(table.String(), want) {
			t.Fatalf("table output missing %q:\n%s", want, table.String())
		}
	}

	var jsonOut bytes.Buffer
	if err := writeEventsJSON(&jsonOut, events); err != nil {
		t.Fatalf("writeEventsJSON returned error: %v", err)
	}
	assertNoSensitiveEventOutput(t, jsonOut.String())
	var decoded []eventRow
	if err := json.Unmarshal(jsonOut.Bytes(), &decoded); err != nil {
		t.Fatalf("event JSON did not decode: %v\n%s", err, jsonOut.String())
	}
	if len(decoded) != len(events) || decoded[0].Event != "delivery.job.current" {
		t.Fatalf("decoded events = %#v", decoded)
	}
}

func TestRunEventsCommandReadsDatabaseWithoutMutating(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "events.db")
	db := openSeededEventDBAtPath(t, dbPath)
	before := tableCounts(t, db)
	db.Close()

	var out bytes.Buffer
	if err := runEventsCommand(&out, []string{"-database", dbPath, "-limit", "2"}); err != nil {
		t.Fatalf("runEventsCommand returned error: %v", err)
	}
	if !strings.Contains(out.String(), "delivery.job.current") {
		t.Fatalf("events output missing current job event:\n%s", out.String())
	}

	db = openExistingEventDB(t, dbPath)
	defer db.Close()
	after := tableCounts(t, db)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("table counts changed: before=%#v after=%#v", before, after)
	}
}

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
			MailtrapAPIURL:   server.URL,
			MailtrapAPIToken: "secret-token",
			MailtrapBCC:      []string{"Audit <audit@example.com>"},
		},
	}

	response, err := app.sendMailtrap(context.Background(), job{
		From:       "Website Contact <contact@example.com>",
		Recipients: []string{"Owner <owner@example.com>"},
		Name:       "Jane Doe",
		Email:      "jane@example.net",
		Subject:    "Subject",
		Message:    "Message body",
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
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	writeTestConfig(t, configPath, `
projects:
  - key: test-project
    name: test
    from: Website Contact <contact@example.com>
    allowed_origins:
      - https://example.com
    recipients:
      - owner@example.com
mail:
  delivery_provider: mailtrap
  mailtrap:
    api_token: yaml-token
security:
  ip_hash_secret: secret
`)
	t.Setenv("DELIVERY_PROVIDER", "mailtrap")
	t.Setenv("MAILTRAP_API_TOKEN", "secret-token")

	cfg, err := loadConfigFromArgs([]string{"--config", configPath})
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
projects:
  - key: yaml-project
    name: yaml-site
    from: Website Contact <contact@example.com>
    allowed_origins:
      - https://example.com
    recipients:
      - Owner <owner@example.com>
  - key: second-project
    name: second-site
    from: Second Contact <second@example.com>
    allowed_origins:
      - https://second.example.com
    recipients:
      - Second Owner <owner2@example.com>
mail:
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
	if len(cfg.Projects) != 2 {
		t.Fatalf("len(Projects) = %d", len(cfg.Projects))
	}
	if cfg.Projects[0].Key != "yaml-project" || cfg.Projects[0].Name != "yaml-site" || cfg.Projects[0].From != "Website Contact <contact@example.com>" {
		t.Fatalf("project[0] = %#v", cfg.Projects[0])
	}
	if cfg.DeliveryProvider != "mailtrap" || cfg.MailtrapAPIToken != "yaml-token" {
		t.Fatalf("mailtrap config = %#v", cfg)
	}
	if cfg.RateMinute != 9 || cfg.RateDay != 90 || cfg.MaxRetries != 7 || cfg.WorkerInterval.Seconds() != 11 || cfg.RetentionDays != 45 {
		t.Fatalf("numeric config = %#v", cfg)
	}
}

func TestConfigExampleYAMLLoads(t *testing.T) {
	clearConfigEnv(t)
	cfg, err := loadConfigFromArgs([]string{"--config", filepath.Join("..", "..", "config.example.yaml")})
	if err != nil {
		t.Fatalf("loadConfigFromArgs returned error for config.example.yaml: %v", err)
	}
	if len(cfg.Projects) != 1 || cfg.Projects[0].Key != "replace-with-public-project-key" {
		t.Fatalf("Projects = %#v", cfg.Projects)
	}
	if cfg.DatabasePath != "relay-house.db" {
		t.Fatalf("DatabasePath = %q", cfg.DatabasePath)
	}
	if cfg.DeliveryProvider != "smtp" || cfg.SMTPHost != "smtp.example.com" {
		t.Fatalf("provider config = %#v", cfg)
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
projects:
  - key: yaml-project
    from: Website Contact <contact@example.com>
    allowed_origins:
      - https://example.com
    recipients:
      - owner@example.com
mail:
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

func TestLoadConfigRejectsMissingProjectFrom(t *testing.T) {
	clearConfigEnv(t)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	writeTestConfig(t, configPath, `
projects:
  - key: yaml-project
    allowed_origins:
      - https://example.com
    recipients:
      - owner@example.com
mail:
  delivery_provider: mailtrap
  mailtrap:
    api_token: yaml-token
security:
  ip_hash_secret: yaml-secret
`)
	_, err := loadConfigFromArgs([]string{"--config", configPath})
	if err == nil || !strings.Contains(err.Error(), "projects[0].from is required") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadConfigRejectsDuplicateProjectKeys(t *testing.T) {
	clearConfigEnv(t)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	writeTestConfig(t, configPath, `
projects:
  - key: duplicate
    from: First <first@example.com>
    allowed_origins:
      - https://example.com
    recipients:
      - owner@example.com
  - key: duplicate
    from: Second <second@example.com>
    allowed_origins:
      - https://second.example.com
    recipients:
      - owner2@example.com
mail:
  delivery_provider: mailtrap
  mailtrap:
    api_token: yaml-token
security:
  ip_hash_secret: yaml-secret
`)
	_, err := loadConfigFromArgs([]string{"--config", configPath})
	if err == nil || !strings.Contains(err.Error(), `duplicate project key "duplicate"`) {
		t.Fatalf("error = %v", err)
	}
}

func TestProjectEnvVarsDoNotConfigureProjects(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("PROJECT_KEY", "test-project")
	t.Setenv("ALLOWED_ORIGINS", "https://example.com")
	t.Setenv("RECIPIENTS", "owner@example.com")
	t.Setenv("MAIL_FROM", "Website Contact <contact@example.com>")
	t.Setenv("DELIVERY_PROVIDER", "mailtrap")
	t.Setenv("MAILTRAP_API_TOKEN", "secret-token")
	t.Setenv("IP_HASH_SECRET", "secret")

	_, err := loadConfig()
	if err == nil || !strings.Contains(err.Error(), "at least one project is required") {
		t.Fatalf("error = %v", err)
	}
}

func TestSendUsesPerProjectOriginPolicy(t *testing.T) {
	app, closeApp := newTestApp(t, []ProjectConfig{
		{
			Key:            "project-a",
			Name:           "Project A",
			From:           "Project A <a@example.com>",
			AllowedOrigins: []string{"https://a.example.com"},
			Recipients:     []string{"Owner A <owner-a@example.com>"},
		},
		{
			Key:            "project-b",
			Name:           "Project B",
			From:           "Project B <b@example.com>",
			AllowedOrigins: []string{"https://b.example.com"},
			Recipients:     []string{"Owner B <owner-b@example.com>"},
		},
	})
	defer closeApp()

	body := `{"project":"project-a","name":"Jane Doe","email":"jane@example.net","subject":"Hello","message":"Message"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/send", strings.NewReader(body))
	req.Header.Set("Origin", "https://a.example.com")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	app.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "https://a.example.com" {
		t.Fatalf("Access-Control-Allow-Origin = %q", rec.Header().Get("Access-Control-Allow-Origin"))
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/send", strings.NewReader(body))
	req.Header.Set("Origin", "https://b.example.com")
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	app.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "origin not allowed") {
		t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/send", strings.NewReader(strings.Replace(body, "project-a", "unknown-project", 1)))
	req.Header.Set("Origin", "https://a.example.com")
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	app.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "unknown project") {
		t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
	}
}

func TestOptionsAllowsOriginFromAnyProject(t *testing.T) {
	app, closeApp := newTestApp(t, []ProjectConfig{
		{
			Key:            "project-a",
			Name:           "Project A",
			From:           "Project A <a@example.com>",
			AllowedOrigins: []string{"https://a.example.com"},
			Recipients:     []string{"Owner A <owner-a@example.com>"},
		},
		{
			Key:            "project-b",
			Name:           "Project B",
			From:           "Project B <b@example.com>",
			AllowedOrigins: []string{"https://b.example.com"},
			Recipients:     []string{"Owner B <owner-b@example.com>"},
		},
	})
	defer closeApp()

	req := httptest.NewRequest(http.MethodOptions, "/v1/send", nil)
	req.Header.Set("Origin", "https://b.example.com")
	rec := httptest.NewRecorder()
	app.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "https://b.example.com" {
		t.Fatalf("Access-Control-Allow-Origin = %q", rec.Header().Get("Access-Control-Allow-Origin"))
	}
}

func TestMigrateAddsProjectFromAddressColumn(t *testing.T) {
	db := openExistingEventDB(t, filepath.Join(t.TempDir(), "old.db"))
	defer db.Close()
	execSQL(t, db, `
		CREATE TABLE projects (
			key TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			recipients_json TEXT NOT NULL,
			allowed_origins_json TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)
	`)
	execSQL(t, db, `
		INSERT INTO projects (key, name, recipients_json, allowed_origins_json, created_at, updated_at)
		VALUES ('project-a', 'old name', '[]', '[]', '2026-07-03T11:59:00Z', '2026-07-03T11:59:00Z')
	`)
	app := &App{
		cfg: Config{
			Projects: []ProjectConfig{
				{
					Key:            "project-a",
					Name:           "Project A",
					From:           "Project A <a@example.com>",
					AllowedOrigins: []string{"https://a.example.com"},
					Recipients:     []string{"Owner A <owner-a@example.com>"},
				},
			},
		},
		db:  db,
		now: func() time.Time { return time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC) },
	}
	if err := app.migrate(context.Background()); err != nil {
		t.Fatalf("migrate returned error: %v", err)
	}
	if err := app.seedProjects(context.Background()); err != nil {
		t.Fatalf("seedProjects returned error: %v", err)
	}
	var from string
	if err := db.QueryRow(`SELECT from_address FROM projects WHERE key = 'project-a'`).Scan(&from); err != nil {
		t.Fatalf("select from_address: %v", err)
	}
	if from != "Project A <a@example.com>" {
		t.Fatalf("from_address = %q", from)
	}
}

func TestSeedProjectsRejectsStoredProjectWithoutFromAddress(t *testing.T) {
	db := openExistingEventDB(t, filepath.Join(t.TempDir(), "old.db"))
	defer db.Close()
	execSQL(t, db, `
		CREATE TABLE projects (
			key TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			recipients_json TEXT NOT NULL,
			allowed_origins_json TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)
	`)
	execSQL(t, db, `
		INSERT INTO projects (key, name, recipients_json, allowed_origins_json, created_at, updated_at)
		VALUES ('old-project', 'old name', '[]', '[]', '2026-07-03T11:59:00Z', '2026-07-03T11:59:00Z')
	`)
	app := &App{
		cfg: Config{
			Projects: []ProjectConfig{
				{
					Key:            "new-project",
					Name:           "Project A",
					From:           "Project A <a@example.com>",
					AllowedOrigins: []string{"https://a.example.com"},
					Recipients:     []string{"Owner A <owner-a@example.com>"},
				},
			},
		},
		db:  db,
		now: func() time.Time { return time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC) },
	}
	if err := app.migrate(context.Background()); err != nil {
		t.Fatalf("migrate returned error: %v", err)
	}
	err := app.seedProjects(context.Background())
	if err == nil || !strings.Contains(err.Error(), "stored projects missing from_address after migration: old-project") {
		t.Fatalf("error = %v", err)
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

func openSeededEventDB(t *testing.T) *sql.DB {
	t.Helper()
	return openSeededEventDBAtPath(t, filepath.Join(t.TempDir(), "events.db"))
}

func openSeededEventDBAtPath(t *testing.T, path string) *sql.DB {
	t.Helper()
	db := openExistingEventDB(t, path)
	app := &App{
		db:  db,
		now: func() time.Time { return time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC) },
	}
	if err := app.migrate(context.Background()); err != nil {
		t.Fatalf("migrate event db: %v", err)
	}
	seedEventRows(t, db)
	return db
}

func newTestApp(t *testing.T, projects []ProjectConfig) (*App, func()) {
	t.Helper()
	raw := defaultConfig()
	raw.Projects = projects
	raw.DeliveryProvider = "mailtrap"
	raw.MailtrapAPIToken = "secret-token"
	raw.RateMinute = 100
	raw.RateDay = 100
	raw.MaxRetries = 5
	raw.IPHashSecret = "hash-secret"
	cfg, err := validateConfig(raw)
	if err != nil {
		t.Fatalf("validate config: %v", err)
	}
	db := openExistingEventDB(t, filepath.Join(t.TempDir(), "relay-house.db"))
	app := &App{
		cfg:        cfg,
		db:         db,
		log:        nilLogger(),
		projectMap: makeProjectMap(cfg.Projects),
		now:        func() time.Time { return time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC) },
	}
	if err := app.migrate(context.Background()); err != nil {
		db.Close()
		t.Fatalf("migrate test app: %v", err)
	}
	if err := app.seedProjects(context.Background()); err != nil {
		db.Close()
		t.Fatalf("seed test app: %v", err)
	}
	return app, func() { _ = db.Close() }
}

func nilLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func openExistingEventDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}
	db.SetMaxOpenConns(1)
	return db
}

func seedEventRows(t *testing.T, db *sql.DB) {
	t.Helper()
	execSQL(t, db, `
		INSERT INTO projects (key, name, recipients_json, allowed_origins_json, created_at, updated_at)
		VALUES ('project-a', 'main', '[]', '[]', '2026-07-03T11:59:00Z', '2026-07-03T11:59:00Z')
	`)
	execSQL(t, db, `
		INSERT INTO submissions (
			id, project_key, origin, ip_hash, user_agent,
			name, email, subject, message, payload_json,
			status, created_at, updated_at
		) VALUES (
			'submission-a', 'project-a', 'https://example.com', 'hash', 'agent',
			'Private Person', 'private@example.com', 'Private Subject', 'Private Message Body',
			'{"message":"Private Message Body"}',
			'retry', '2026-07-03T12:00:00Z', '2026-07-03T12:03:00Z'
		)
	`)
	execSQL(t, db, `
		INSERT INTO delivery_jobs (
			id, submission_id, status, attempt_count, next_attempt_at, last_error, created_at, updated_at
		) VALUES (
			'job-a', 'submission-a', 'retry', 2, '2026-07-03T12:10:00Z',
			'smtp temporary failure', '2026-07-03T12:00:00Z', '2026-07-03T12:03:00Z'
		)
	`)
	execSQL(t, db, `
		INSERT INTO delivery_attempts (
			id, job_id, submission_id, attempt_number, status, provider,
			response, error, duration_ms, created_at
		) VALUES (
			'attempt-1', 'job-a', 'submission-a', 1, 'sent', 'smtp',
			'accepted by smtp client', NULL, 25, '2026-07-03T12:01:00Z'
		)
	`)
	execSQL(t, db, `
		INSERT INTO delivery_attempts (
			id, job_id, submission_id, attempt_number, status, provider,
			response, error, duration_ms, created_at
		) VALUES (
			'attempt-2', 'job-a', 'submission-a', 2, 'failed', 'smtp',
			NULL, 'smtp temporary failure', 30, '2026-07-03T12:02:00Z'
		)
	`)
}

func execSQL(t *testing.T, db *sql.DB, query string) {
	t.Helper()
	if _, err := db.Exec(query); err != nil {
		t.Fatalf("exec SQL failed: %v\n%s", err, query)
	}
}

func tableCounts(t *testing.T, db *sql.DB) map[string]int {
	t.Helper()
	counts := make(map[string]int)
	for _, table := range []string{"projects", "submissions", "delivery_jobs", "delivery_attempts", "rate_limits"} {
		var count int
		if err := db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		counts[table] = count
	}
	return counts
}

func assertNoSensitiveEventOutput(t *testing.T, output string) {
	t.Helper()
	for _, disallowed := range []string{
		"Private Person",
		"private@example.com",
		"Private Subject",
		"Private Message Body",
	} {
		if strings.Contains(output, disallowed) {
			t.Fatalf("event output leaked %q:\n%s", disallowed, output)
		}
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
		"RELAY_HOUSE_CONFIG",
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
