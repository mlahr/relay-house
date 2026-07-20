package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHealthPassesProvidersWithoutDeliveryHistory(t *testing.T) {
	providers := []ProviderConfig{
		{Name: "smtp-main", Type: "smtp"},
		{Name: "mailtrap-main", Type: "mailtrap", MailtrapAPIToken: "mailtrap-secret"},
		{Name: "telegram-main", Type: "telegram", TelegramBotToken: "telegram-secret"},
	}
	app, closeApp := newHealthTestApp(t, providers)
	defer closeApp()

	var probeMu sync.Mutex
	var probed []string
	app.probeHealth = func(_ context.Context, provider ProviderConfig) providerLiveHealth {
		probeMu.Lock()
		defer probeMu.Unlock()
		probed = append(probed, provider.Name)
		return providerLiveHealth{Status: liveCheckOK}
	}

	status, response, body := requestHealth(t, app)
	if status != http.StatusOK || response.Status != healthOK {
		t.Fatalf("status/response = %d %#v, body=%s", status, response, body)
	}
	probeMu.Lock()
	if len(probed) != 2 || !containsString(probed, "smtp-main") || !containsString(probed, "telegram-main") {
		t.Fatalf("probed providers = %#v", probed)
	}
	probeMu.Unlock()
	if len(response.Providers) != 3 {
		t.Fatalf("providers = %#v", response.Providers)
	}
	for i, provider := range response.Providers {
		if provider.Name != providers[i].Name {
			t.Fatalf("provider order = %#v", response.Providers)
		}
		if provider.Status != healthOK || provider.LastAttempt != lastAttemptNone {
			t.Fatalf("provider = %#v", provider)
		}
	}
	if response.Providers[1].LiveCheck != liveCheckNotPerformed || response.Providers[1].LiveCheckedAt != "" {
		t.Fatalf("Mailtrap provider = %#v", response.Providers[1])
	}
	for _, secret := range []string{"mailtrap-secret", "telegram-secret"} {
		if strings.Contains(body, secret) {
			t.Fatalf("health response contains secret %q: %s", secret, body)
		}
	}
}

func TestHealthUsesLatestAttemptPerProvider(t *testing.T) {
	app, closeApp := newHealthTestApp(t, []ProviderConfig{{Name: "mailtrap-main", Type: "mailtrap"}})
	defer closeApp()

	insertHealthAttempt(t, app.db, "mailtrap-main", "sent", "2026-07-20T09:00:00Z", 1)
	insertHealthAttempt(t, app.db, "mailtrap-main", "failed", "2026-07-20T10:00:00Z", 2)

	status, response, _ := requestHealth(t, app)
	if status != http.StatusServiceUnavailable || response.Status != healthUnhealthy {
		t.Fatalf("status/response = %d %#v", status, response)
	}
	provider := response.Providers[0]
	if provider.LastAttempt != lastAttemptFailed || provider.LastAttemptAt != "2026-07-20T10:00:00Z" {
		t.Fatalf("provider = %#v", provider)
	}
	if !containsString(provider.ReasonCodes, reasonLatestAttemptFailed) {
		t.Fatalf("reason codes = %#v", provider.ReasonCodes)
	}

	insertHealthAttempt(t, app.db, "mailtrap-main", "sent", "2026-07-20T11:00:00Z", 3)
	status, response, _ = requestHealth(t, app)
	if status != http.StatusOK || response.Providers[0].LastAttempt != lastAttemptSucceeded {
		t.Fatalf("status/response = %d %#v", status, response)
	}
}

func TestHealthFailsGloballyAndCachesLiveProviderChecks(t *testing.T) {
	providers := []ProviderConfig{
		{Name: "smtp-main", Type: "smtp"},
		{Name: "telegram-main", Type: "telegram"},
		{Name: "mailtrap-main", Type: "mailtrap"},
	}
	app, closeApp := newHealthTestApp(t, providers)
	defer closeApp()

	now := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	app.now = func() time.Time { return now }
	var countMu sync.Mutex
	counts := make(map[string]int)
	app.probeHealth = func(_ context.Context, provider ProviderConfig) providerLiveHealth {
		countMu.Lock()
		counts[provider.Name]++
		countMu.Unlock()
		if provider.Name == "telegram-main" {
			return failedLiveHealth(reasonAuthenticationFailed)
		}
		return providerLiveHealth{Status: liveCheckOK}
	}

	for range 2 {
		status, response, _ := requestHealth(t, app)
		if status != http.StatusServiceUnavailable || response.Error != "delivery provider unavailable" {
			t.Fatalf("status/response = %d %#v", status, response)
		}
		if response.Providers[0].Status != healthOK || response.Providers[1].Status != healthUnhealthy || response.Providers[2].Status != healthOK {
			t.Fatalf("providers = %#v", response.Providers)
		}
	}
	countMu.Lock()
	if counts["smtp-main"] != 1 || counts["telegram-main"] != 1 || counts["mailtrap-main"] != 0 {
		t.Fatalf("probe counts = %#v", counts)
	}
	countMu.Unlock()

	now = now.Add(healthCacheTTL + time.Second)
	_, _, _ = requestHealth(t, app)
	countMu.Lock()
	if counts["smtp-main"] != 2 || counts["telegram-main"] != 2 || counts["mailtrap-main"] != 0 {
		t.Fatalf("probe counts after expiry = %#v", counts)
	}
	countMu.Unlock()
}

func TestHealthCoalescesConcurrentLiveProbeRefresh(t *testing.T) {
	app, closeApp := newHealthTestApp(t, []ProviderConfig{{Name: "smtp-main", Type: "smtp"}})
	defer closeApp()

	var mu sync.Mutex
	probeCount := 0
	started := make(chan struct{})
	release := make(chan struct{})
	app.probeHealth = func(_ context.Context, _ ProviderConfig) providerLiveHealth {
		mu.Lock()
		probeCount++
		if probeCount == 1 {
			close(started)
		}
		mu.Unlock()
		<-release
		return providerLiveHealth{Status: liveCheckOK}
	}

	const requests = 5
	var wg sync.WaitGroup
	wg.Add(requests)
	statuses := make(chan int, requests)
	for range requests {
		go func() {
			defer wg.Done()
			status, _, _ := requestHealth(t, app)
			statuses <- status
		}()
	}
	<-started
	close(release)
	wg.Wait()
	close(statuses)

	for status := range statuses {
		if status != http.StatusOK {
			t.Fatalf("status = %d", status)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if probeCount != 1 {
		t.Fatalf("probe count = %d, want 1", probeCount)
	}
}

func TestHealthReportsUnavailableDatabaseAndUnknownHistory(t *testing.T) {
	app, closeApp := newHealthTestApp(t, []ProviderConfig{{Name: "mailtrap-main", Type: "mailtrap"}})
	closeApp()

	status, response, _ := requestHealth(t, app)
	if status != http.StatusServiceUnavailable || response.Database.Status != healthUnhealthy {
		t.Fatalf("status/response = %d %#v", status, response)
	}
	if !containsString(response.Database.ReasonCodes, reasonDatabaseUnavailable) {
		t.Fatalf("database = %#v", response.Database)
	}
	provider := response.Providers[0]
	if provider.Status != healthUnknown || provider.LastAttempt != lastAttemptUnknown || !containsString(provider.ReasonCodes, reasonHistoryUnavailable) {
		t.Fatalf("provider = %#v", provider)
	}
}

func TestHealthReportsUnavailableDeliveryHistory(t *testing.T) {
	app, closeApp := newHealthTestApp(t, []ProviderConfig{{Name: "mailtrap-main", Type: "mailtrap"}})
	defer closeApp()
	if _, err := app.db.Exec(`DROP TABLE delivery_attempts`); err != nil {
		t.Fatalf("drop delivery_attempts: %v", err)
	}

	status, response, _ := requestHealth(t, app)
	if status != http.StatusServiceUnavailable || response.Database.Status != healthUnhealthy {
		t.Fatalf("status/response = %d %#v", status, response)
	}
	if !containsString(response.Database.ReasonCodes, reasonHistoryUnavailable) {
		t.Fatalf("database = %#v", response.Database)
	}
	if response.Providers[0].LastAttempt != lastAttemptUnknown {
		t.Fatalf("provider = %#v", response.Providers[0])
	}
}

func TestProbeTelegramHealth(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		wantStatus string
		wantReason string
	}{
		{name: "success", statusCode: http.StatusOK, body: `{"ok":true,"result":{"id":1}}`, wantStatus: liveCheckOK},
		{name: "unauthorized", statusCode: http.StatusUnauthorized, body: `{"ok":false}`, wantStatus: liveCheckFailed, wantReason: reasonAuthenticationFailed},
		{name: "api rejection", statusCode: http.StatusOK, body: `{"ok":false,"description":"bad token"}`, wantStatus: liveCheckFailed, wantReason: reasonAuthenticationFailed},
		{name: "malformed response", statusCode: http.StatusOK, body: `{`, wantStatus: liveCheckFailed, wantReason: reasonUnexpectedResponse},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotMethod, gotPath string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod, gotPath = r.Method, r.URL.Path
				w.WriteHeader(tt.statusCode)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()

			result := probeTelegramHealth(context.Background(), ProviderConfig{
				Type:             "telegram",
				TelegramAPIBase:  server.URL,
				TelegramBotToken: "secret-token",
			})
			if result.Status != tt.wantStatus || (tt.wantReason != "" && !containsString(result.ReasonCodes, tt.wantReason)) {
				t.Fatalf("result = %#v", result)
			}
			if gotMethod != http.MethodGet || gotPath != "/botsecret-token/getMe" {
				t.Fatalf("request = %s %s", gotMethod, gotPath)
			}
		})
	}
}

func TestProbeTelegramHealthReportsConnectivityAndTimeout(t *testing.T) {
	closedServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closedURL := closedServer.URL
	closedServer.Close()
	result := probeTelegramHealth(context.Background(), ProviderConfig{
		TelegramAPIBase:  closedURL,
		TelegramBotToken: "secret-token",
	})
	if result.Status != liveCheckFailed || !containsString(result.ReasonCodes, reasonConnectivityFailed) {
		t.Fatalf("connectivity result = %#v", result)
	}

	timeoutServer := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer timeoutServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	result = probeTelegramHealth(ctx, ProviderConfig{
		TelegramAPIBase:  timeoutServer.URL,
		TelegramBotToken: "secret-token",
	})
	if result.Status != liveCheckFailed || !containsString(result.ReasonCodes, reasonProbeTimeout) {
		t.Fatalf("timeout result = %#v", result)
	}
}

func TestProbeSMTPHealthAuthenticatesWithoutMailTransaction(t *testing.T) {
	address, commands, closeServer := startTestSMTPServer(t, true)
	defer closeServer()
	_, portText, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}

	result := probeSMTPHealth(context.Background(), ProviderConfig{
		Type:         "smtp",
		SMTPHost:     "localhost",
		SMTPPort:     port,
		SMTPUsername: "user",
		SMTPPassword: "password",
	})
	if result.Status != liveCheckOK {
		t.Fatalf("result = %#v, commands = %#v", result, commands())
	}
	joined := strings.Join(commands(), "\n")
	if !strings.Contains(joined, "AUTH PLAIN") {
		t.Fatalf("commands missing authentication: %s", joined)
	}
	for _, forbidden := range []string{"MAIL FROM", "RCPT TO", "DATA"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("commands contain %q: %s", forbidden, joined)
		}
	}
}

func TestProbeSMTPHealthReportsAuthenticationFailure(t *testing.T) {
	address, _, closeServer := startTestSMTPServer(t, false)
	defer closeServer()
	_, portText, _ := net.SplitHostPort(address)
	port, _ := strconv.Atoi(portText)

	result := probeSMTPHealth(context.Background(), ProviderConfig{
		Type:         "smtp",
		SMTPHost:     "localhost",
		SMTPPort:     port,
		SMTPUsername: "user",
		SMTPPassword: "wrong",
	})
	if result.Status != liveCheckFailed || !containsString(result.ReasonCodes, reasonAuthenticationFailed) {
		t.Fatalf("result = %#v", result)
	}
}

func newHealthTestApp(t *testing.T, providers []ProviderConfig) (*App, func()) {
	t.Helper()
	db := openExistingEventDB(t, t.TempDir()+"/health.db")
	app := &App{
		cfg: Config{Providers: providers},
		db:  db,
		log: nilLogger(),
		now: func() time.Time { return time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC) },
	}
	if err := app.migrate(context.Background()); err != nil {
		db.Close()
		t.Fatalf("migrate health db: %v", err)
	}
	return app, func() { _ = db.Close() }
}

func insertHealthAttempt(t *testing.T, db *sql.DB, provider, status, createdAt string, sequence int) {
	t.Helper()
	projectID := fmt.Sprintf("project-%d", sequence)
	submissionID := fmt.Sprintf("submission-%d", sequence)
	jobID := fmt.Sprintf("job-%d", sequence)
	attemptID := fmt.Sprintf("attempt-%d", sequence)
	if _, err := db.Exec(`INSERT INTO projects (key, name, from_address, recipients_json, allowed_origins_json, created_at, updated_at) VALUES (?, ?, '', '[]', '[]', ?, ?)`, projectID, projectID, createdAt, createdAt); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO submissions (id, project_key, origin, ip_hash, user_agent, name, email, subject, message, payload_json, status, created_at, updated_at) VALUES (?, ?, '', '', '', '', '', '', '', '{}', ?, ?, ?)`, submissionID, projectID, status, createdAt, createdAt); err != nil {
		t.Fatalf("insert submission: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO delivery_jobs (id, submission_id, status, attempt_count, next_attempt_at, created_at, updated_at) VALUES (?, ?, ?, 1, ?, ?, ?)`, jobID, submissionID, status, createdAt, createdAt, createdAt); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO delivery_attempts (id, job_id, submission_id, attempt_number, status, provider, duration_ms, created_at) VALUES (?, ?, ?, 1, ?, ?, 1, ?)`, attemptID, jobID, submissionID, status, provider, createdAt); err != nil {
		t.Fatalf("insert attempt: %v", err)
	}
}

func requestHealth(t *testing.T, app *App) (int, healthResponse, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	app.routes().ServeHTTP(rec, req)
	body := rec.Body.String()
	var response healthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode health response: %v; body=%s", err, body)
	}
	return rec.Code, response, body
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func startTestSMTPServer(t *testing.T, acceptAuth bool) (string, func() []string, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	var mu sync.Mutex
	var commands []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = fmt.Fprint(conn, "220 localhost ESMTP\r\n")
		reader := bufio.NewReader(conn)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSpace(line)
			mu.Lock()
			commands = append(commands, line)
			mu.Unlock()
			switch {
			case strings.HasPrefix(line, "EHLO"):
				_, _ = fmt.Fprint(conn, "250-localhost\r\n250 AUTH PLAIN\r\n")
			case strings.HasPrefix(line, "AUTH PLAIN") && acceptAuth:
				_, _ = fmt.Fprint(conn, "235 2.7.0 Authentication successful\r\n")
			case strings.HasPrefix(line, "AUTH PLAIN"):
				_, _ = fmt.Fprint(conn, "535 5.7.8 Authentication failed\r\n")
			case strings.HasPrefix(line, "QUIT"):
				_, _ = fmt.Fprint(conn, "221 2.0.0 Bye\r\n")
				return
			default:
				_, _ = fmt.Fprint(conn, "500 5.5.2 Command not recognized\r\n")
			}
		}
	}()

	readCommands := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), commands...)
	}
	closeServer := func() {
		_ = listener.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("SMTP test server did not stop")
		}
	}
	return listener.Addr().String(), readCommands, closeServer
}
