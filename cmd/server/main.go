package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

type Config struct {
	Addr              string
	DatabasePath      string
	ProjectKey        string
	ProjectName       string
	AllowedOrigins    []string
	Recipients        []string
	From              string
	SMTPHost          string
	SMTPPort          int
	SMTPUsername      string
	SMTPPassword      string
	SMTPInsecurePlain bool
	TurnstileSecret   string
	RateMinute        int
	RateDay           int
	MaxRetries        int
	WorkerInterval    time.Duration
	RetentionDays     int
	IPHashSecret      string
}

type App struct {
	cfg       Config
	db        *sql.DB
	log       *slog.Logger
	originSet map[string]struct{}
	now       func() time.Time
}

type SendRequest struct {
	Project        string `json:"project"`
	Name           string `json:"name"`
	Email          string `json:"email"`
	Subject        string `json:"subject"`
	Message        string `json:"message"`
	TurnstileToken string `json:"turnstileToken"`
}

type SendResponse struct {
	ID     string `json:"id,omitempty"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

type job struct {
	ID           string
	SubmissionID string
	ProjectKey   string
	Name         string
	Email        string
	Subject      string
	Message      string
	AttemptCount int
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg, err := loadConfig()
	if err != nil {
		logger.Error("config.invalid", "error", err)
		os.Exit(1)
	}

	db, err := sql.Open("sqlite", cfg.DatabasePath+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)")
	if err != nil {
		logger.Error("db.open_failed", "error", err)
		os.Exit(1)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	app := &App{
		cfg:       cfg,
		db:        db,
		log:       logger,
		originSet: makeOriginSet(cfg.AllowedOrigins),
		now:       time.Now,
	}

	if err := app.migrate(context.Background()); err != nil {
		logger.Error("db.migrate_failed", "error", err)
		os.Exit(1)
	}
	if err := app.seedProject(context.Background()); err != nil {
		logger.Error("db.seed_failed", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		app.worker(ctx)
	}()

	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           app.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		logger.Info("server.started", "addr", cfg.Addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server.failed", "error", err)
			stop()
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("server.shutdown_failed", "error", err)
	}
	wg.Wait()
	logger.Info("server.stopped")
}

func loadConfig() (Config, error) {
	cfg := Config{
		Addr:            env("ADDR", ":8080"),
		DatabasePath:    env("DATABASE_PATH", "email-endpoint.db"),
		ProjectKey:      env("PROJECT_KEY", ""),
		ProjectName:     env("PROJECT_NAME", "default"),
		AllowedOrigins:  splitCSV(env("ALLOWED_ORIGINS", "")),
		Recipients:      splitCSV(env("RECIPIENTS", "")),
		From:            env("MAIL_FROM", ""),
		SMTPHost:        env("SMTP_HOST", ""),
		SMTPPort:        envInt("SMTP_PORT", 587),
		SMTPUsername:    env("SMTP_USERNAME", ""),
		SMTPPassword:    env("SMTP_PASSWORD", ""),
		TurnstileSecret: env("TURNSTILE_SECRET", ""),
		RateMinute:      envInt("RATE_LIMIT_PER_MINUTE", 5),
		RateDay:         envInt("RATE_LIMIT_PER_DAY", 100),
		MaxRetries:      envInt("MAX_RETRIES", 5),
		WorkerInterval:  time.Duration(envInt("WORKER_INTERVAL_SECONDS", 5)) * time.Second,
		RetentionDays:   envInt("RETENTION_DAYS", 365),
		IPHashSecret:    env("IP_HASH_SECRET", ""),
	}
	cfg.SMTPInsecurePlain = envBool("SMTP_INSECURE_PLAIN_AUTH", false)

	if cfg.ProjectKey == "" {
		generated, err := randomToken(24)
		if err != nil {
			return Config{}, err
		}
		cfg.ProjectKey = generated
	}
	if len(cfg.AllowedOrigins) == 0 {
		return Config{}, errors.New("ALLOWED_ORIGINS is required")
	}
	if len(cfg.Recipients) == 0 {
		return Config{}, errors.New("RECIPIENTS is required")
	}
	if cfg.From == "" {
		return Config{}, errors.New("MAIL_FROM is required")
	}
	if cfg.SMTPHost == "" {
		return Config{}, errors.New("SMTP_HOST is required")
	}
	if cfg.SMTPUsername == "" || cfg.SMTPPassword == "" {
		return Config{}, errors.New("SMTP_USERNAME and SMTP_PASSWORD are required")
	}
	if cfg.IPHashSecret == "" {
		generated, err := randomToken(32)
		if err != nil {
			return Config{}, err
		}
		cfg.IPHashSecret = generated
	}
	return cfg, nil
}

func (a *App) routes() http.Handler {
	r := chi.NewRouter()
	r.Use(a.requestLog)
	r.Get("/healthz", a.health)
	r.Options("/v1/send", a.options)
	r.Post("/v1/send", a.send)
	return r
}

func (a *App) requestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := a.now()
		next.ServeHTTP(w, r)
		a.log.Info("http.request", "method", r.Method, "path", r.URL.Path, "duration_ms", time.Since(start).Milliseconds())
	})
}

func (a *App) health(w http.ResponseWriter, r *http.Request) {
	if err := a.db.PingContext(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, SendResponse{Status: "unhealthy", Error: "database unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *App) options(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if a.originAllowed(origin) {
		setCORS(w, origin)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) send(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if !a.originAllowed(origin) {
		a.log.Warn("request.origin_rejected", "origin", origin)
		writeJSON(w, http.StatusForbidden, SendResponse{Status: "rejected", Error: "origin not allowed"})
		return
	}
	setCORS(w, origin)

	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	defer r.Body.Close()

	var req SendRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, SendResponse{Status: "rejected", Error: "invalid JSON body"})
		return
	}

	if err := validateSendRequest(req); err != nil {
		writeJSON(w, http.StatusBadRequest, SendResponse{Status: "rejected", Error: err.Error()})
		return
	}
	if req.Project != a.cfg.ProjectKey {
		writeJSON(w, http.StatusForbidden, SendResponse{Status: "rejected", Error: "unknown project"})
		return
	}

	ip := clientIP(r)
	ipHash := a.hashIP(ip)
	if ok, err := a.checkRateLimit(r.Context(), req.Project, ipHash); err != nil {
		a.log.Error("rate_limit.failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, SendResponse{Status: "error", Error: "rate limit unavailable"})
		return
	} else if !ok {
		writeJSON(w, http.StatusTooManyRequests, SendResponse{Status: "rejected", Error: "rate limit exceeded"})
		return
	}

	if a.cfg.TurnstileSecret != "" {
		if err := a.verifyTurnstile(r.Context(), req.TurnstileToken, ip); err != nil {
			a.log.Warn("turnstile.rejected", "error", err)
			writeJSON(w, http.StatusForbidden, SendResponse{Status: "rejected", Error: "captcha verification failed"})
			return
		}
	}

	submissionID, err := a.storeSubmission(r.Context(), req, origin, ipHash, r.UserAgent())
	if err != nil {
		a.log.Error("submission.store_failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, SendResponse{Status: "error", Error: "could not store submission"})
		return
	}

	a.log.Info("submission.accepted", "submission_id", submissionID, "project", req.Project, "origin", origin)
	writeJSON(w, http.StatusAccepted, SendResponse{ID: submissionID, Status: "queued"})
}

func validateSendRequest(req SendRequest) error {
	req.Name = strings.TrimSpace(req.Name)
	req.Email = strings.TrimSpace(req.Email)
	req.Subject = strings.TrimSpace(req.Subject)
	req.Message = strings.TrimSpace(req.Message)

	if req.Project == "" {
		return errors.New("project is required")
	}
	if len(req.Name) < 1 || len(req.Name) > 120 {
		return errors.New("name must be 1-120 characters")
	}
	if containsHeaderBreak(req.Name) {
		return errors.New("name contains invalid characters")
	}
	if len(req.Email) > 254 {
		return errors.New("email is too long")
	}
	if _, err := mail.ParseAddress(req.Email); err != nil {
		return errors.New("email is invalid")
	}
	if len(req.Subject) < 1 || len(req.Subject) > 180 {
		return errors.New("subject must be 1-180 characters")
	}
	if containsHeaderBreak(req.Subject) {
		return errors.New("subject contains invalid characters")
	}
	if len(req.Message) < 1 || len(req.Message) > 10000 {
		return errors.New("message must be 1-10000 characters")
	}
	return nil
}

func containsHeaderBreak(value string) bool {
	return strings.ContainsAny(value, "\r\n")
}

func (a *App) migrate(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS projects (
			key TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			recipients_json TEXT NOT NULL,
			allowed_origins_json TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS submissions (
			id TEXT PRIMARY KEY,
			project_key TEXT NOT NULL REFERENCES projects(key) ON DELETE CASCADE,
			origin TEXT NOT NULL,
			ip_hash TEXT NOT NULL,
			user_agent TEXT NOT NULL,
			name TEXT NOT NULL,
			email TEXT NOT NULL,
			subject TEXT NOT NULL,
			message TEXT NOT NULL,
			payload_json TEXT NOT NULL,
			status TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS delivery_jobs (
			id TEXT PRIMARY KEY,
			submission_id TEXT NOT NULL REFERENCES submissions(id) ON DELETE CASCADE,
			status TEXT NOT NULL,
			attempt_count INTEGER NOT NULL DEFAULT 0,
			next_attempt_at TEXT NOT NULL,
			last_error TEXT,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS delivery_attempts (
			id TEXT PRIMARY KEY,
			job_id TEXT NOT NULL REFERENCES delivery_jobs(id) ON DELETE CASCADE,
			submission_id TEXT NOT NULL REFERENCES submissions(id) ON DELETE CASCADE,
			attempt_number INTEGER NOT NULL,
			status TEXT NOT NULL,
			provider TEXT NOT NULL,
			response TEXT,
			error TEXT,
			duration_ms INTEGER NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS rate_limits (
			scope TEXT NOT NULL,
			key TEXT NOT NULL,
			window_start TEXT NOT NULL,
			count INTEGER NOT NULL,
			PRIMARY KEY (scope, key, window_start)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_delivery_jobs_due ON delivery_jobs(status, next_attempt_at)`,
		`CREATE INDEX IF NOT EXISTS idx_submissions_created ON submissions(created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_rate_limits_window ON rate_limits(window_start)`,
	}
	for _, stmt := range statements {
		if _, err := a.db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) seedProject(ctx context.Context) error {
	now := a.now().UTC().Format(time.RFC3339Nano)
	recipients, err := json.Marshal(a.cfg.Recipients)
	if err != nil {
		return err
	}
	origins, err := json.Marshal(a.cfg.AllowedOrigins)
	if err != nil {
		return err
	}
	_, err = a.db.ExecContext(ctx, `
		INSERT INTO projects (key, name, recipients_json, allowed_origins_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET
			name = excluded.name,
			recipients_json = excluded.recipients_json,
			allowed_origins_json = excluded.allowed_origins_json,
			updated_at = excluded.updated_at
	`, a.cfg.ProjectKey, a.cfg.ProjectName, string(recipients), string(origins), now, now)
	return err
}

func (a *App) storeSubmission(ctx context.Context, req SendRequest, origin, ipHash, userAgent string) (string, error) {
	now := a.now().UTC().Format(time.RFC3339Nano)
	submissionID := uuid.NewString()
	jobID := uuid.NewString()
	payload, err := json.Marshal(req)
	if err != nil {
		return "", err
	}

	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO submissions (
			id, project_key, origin, ip_hash, user_agent,
			name, email, subject, message, payload_json,
			status, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'queued', ?, ?)
	`, submissionID, req.Project, origin, ipHash, userAgent, strings.TrimSpace(req.Name), strings.TrimSpace(req.Email), strings.TrimSpace(req.Subject), strings.TrimSpace(req.Message), string(payload), now, now)
	if err != nil {
		return "", err
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO delivery_jobs (
			id, submission_id, status, attempt_count, next_attempt_at, created_at, updated_at
		) VALUES (?, ?, 'queued', 0, ?, ?, ?)
	`, jobID, submissionID, now, now, now)
	if err != nil {
		return "", err
	}

	if err := tx.Commit(); err != nil {
		return "", err
	}
	return submissionID, nil
}

func (a *App) checkRateLimit(ctx context.Context, project, ipHash string) (bool, error) {
	now := a.now().UTC()
	minute := now.Truncate(time.Minute)
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	if ok, err := incrementLimit(ctx, tx, "minute", project+":"+ipHash, minute, a.cfg.RateMinute); err != nil || !ok {
		return ok, err
	}
	if ok, err := incrementLimit(ctx, tx, "day", project+":"+ipHash, day, a.cfg.RateDay); err != nil || !ok {
		return ok, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM rate_limits WHERE window_start < ?`, now.Add(-48*time.Hour).Format(time.RFC3339Nano)); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func incrementLimit(ctx context.Context, tx *sql.Tx, scope, key string, window time.Time, limit int) (bool, error) {
	windowText := window.Format(time.RFC3339Nano)
	_, err := tx.ExecContext(ctx, `
		INSERT INTO rate_limits (scope, key, window_start, count)
		VALUES (?, ?, ?, 1)
		ON CONFLICT(scope, key, window_start) DO UPDATE SET count = count + 1
	`, scope, key, windowText)
	if err != nil {
		return false, err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count FROM rate_limits WHERE scope = ? AND key = ? AND window_start = ?`, scope, key, windowText).Scan(&count); err != nil {
		return false, err
	}
	return count <= limit, nil
}

func (a *App) worker(ctx context.Context) {
	ticker := time.NewTicker(a.cfg.WorkerInterval)
	defer ticker.Stop()
	for {
		a.processDueJobs(ctx)
		a.cleanupOldSubmissions(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *App) processDueJobs(ctx context.Context) {
	for {
		j, ok, err := a.claimJob(ctx)
		if err != nil {
			a.log.Error("job.claim_failed", "error", err)
			return
		}
		if !ok {
			return
		}
		a.deliverJob(ctx, j)
	}
}

func (a *App) claimJob(ctx context.Context) (job, bool, error) {
	now := a.now().UTC().Format(time.RFC3339Nano)
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return job{}, false, err
	}
	defer tx.Rollback()

	var id string
	err = tx.QueryRowContext(ctx, `
		SELECT id FROM delivery_jobs
		WHERE status IN ('queued', 'retry') AND next_attempt_at <= ?
		ORDER BY next_attempt_at ASC
		LIMIT 1
	`, now).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return job{}, false, nil
	}
	if err != nil {
		return job{}, false, err
	}

	_, err = tx.ExecContext(ctx, `UPDATE delivery_jobs SET status = 'sending', updated_at = ? WHERE id = ?`, now, id)
	if err != nil {
		return job{}, false, err
	}

	var j job
	err = tx.QueryRowContext(ctx, `
		SELECT
			j.id, j.submission_id, s.project_key, s.name, s.email, s.subject, s.message, j.attempt_count
		FROM delivery_jobs j
		JOIN submissions s ON s.id = j.submission_id
		WHERE j.id = ?
	`, id).Scan(&j.ID, &j.SubmissionID, &j.ProjectKey, &j.Name, &j.Email, &j.Subject, &j.Message, &j.AttemptCount)
	if err != nil {
		return job{}, false, err
	}

	if err := tx.Commit(); err != nil {
		return job{}, false, err
	}
	return j, true, nil
}

func (a *App) deliverJob(ctx context.Context, j job) {
	start := a.now()
	attemptNumber := j.AttemptCount + 1
	err := a.sendSMTP(j)
	duration := time.Since(start).Milliseconds()

	now := a.now().UTC().Format(time.RFC3339Nano)
	status := "sent"
	errorText := ""
	if err != nil {
		status = "failed"
		errorText = err.Error()
	}

	tx, txErr := a.db.BeginTx(ctx, nil)
	if txErr != nil {
		a.log.Error("job.tx_failed", "job_id", j.ID, "error", txErr)
		return
	}
	defer tx.Rollback()

	_, txErr = tx.ExecContext(ctx, `
		INSERT INTO delivery_attempts (
			id, job_id, submission_id, attempt_number, status, provider,
			response, error, duration_ms, created_at
		) VALUES (?, ?, ?, ?, ?, 'smtp', ?, ?, ?, ?)
	`, uuid.NewString(), j.ID, j.SubmissionID, attemptNumber, status, successfulResponse(err), nullableString(errorText), duration, now)
	if txErr != nil {
		a.log.Error("attempt.store_failed", "job_id", j.ID, "error", txErr)
		return
	}

	if err == nil {
		_, txErr = tx.ExecContext(ctx, `UPDATE delivery_jobs SET status = 'sent', attempt_count = ?, last_error = NULL, updated_at = ? WHERE id = ?`, attemptNumber, now, j.ID)
		if txErr == nil {
			_, txErr = tx.ExecContext(ctx, `UPDATE submissions SET status = 'sent', updated_at = ? WHERE id = ?`, now, j.SubmissionID)
		}
	} else if attemptNumber >= a.cfg.MaxRetries {
		_, txErr = tx.ExecContext(ctx, `UPDATE delivery_jobs SET status = 'dead', attempt_count = ?, last_error = ?, updated_at = ? WHERE id = ?`, attemptNumber, errorText, now, j.ID)
		if txErr == nil {
			_, txErr = tx.ExecContext(ctx, `UPDATE submissions SET status = 'dead', updated_at = ? WHERE id = ?`, now, j.SubmissionID)
		}
	} else {
		next := a.now().UTC().Add(backoff(attemptNumber)).Format(time.RFC3339Nano)
		_, txErr = tx.ExecContext(ctx, `UPDATE delivery_jobs SET status = 'retry', attempt_count = ?, next_attempt_at = ?, last_error = ?, updated_at = ? WHERE id = ?`, attemptNumber, next, errorText, now, j.ID)
		if txErr == nil {
			_, txErr = tx.ExecContext(ctx, `UPDATE submissions SET status = 'retry', updated_at = ? WHERE id = ?`, now, j.SubmissionID)
		}
	}
	if txErr != nil {
		a.log.Error("job.update_failed", "job_id", j.ID, "error", txErr)
		return
	}
	if txErr := tx.Commit(); txErr != nil {
		a.log.Error("job.commit_failed", "job_id", j.ID, "error", txErr)
		return
	}

	if err != nil {
		a.log.Warn("email.delivery_failed", "job_id", j.ID, "submission_id", j.SubmissionID, "attempt", attemptNumber, "error", err)
		return
	}
	a.log.Info("email.sent", "job_id", j.ID, "submission_id", j.SubmissionID, "duration_ms", duration)
}

func (a *App) sendSMTP(j job) error {
	fromAddr, err := mail.ParseAddress(a.cfg.From)
	if err != nil {
		return fmt.Errorf("invalid MAIL_FROM: %w", err)
	}
	replyTo, err := mail.ParseAddress(fmt.Sprintf("%s <%s>", j.Name, j.Email))
	if err != nil {
		return fmt.Errorf("invalid reply-to: %w", err)
	}

	var msg bytes.Buffer
	writeHeader(&msg, "From", fromAddr.String())
	writeHeader(&msg, "To", strings.Join(a.cfg.Recipients, ", "))
	writeHeader(&msg, "Reply-To", replyTo.String())
	writeHeader(&msg, "Subject", j.Subject)
	writeHeader(&msg, "MIME-Version", "1.0")
	writeHeader(&msg, "Content-Type", `text/plain; charset="utf-8"`)
	writeHeader(&msg, "Content-Transfer-Encoding", "8bit")
	msg.WriteString("\r\n")
	msg.WriteString(j.Message)
	msg.WriteString("\r\n")

	auth := smtp.PlainAuth("", a.cfg.SMTPUsername, a.cfg.SMTPPassword, a.cfg.SMTPHost)
	if a.cfg.SMTPPort == 465 {
		return sendMailImplicitTLS(a.cfg.SMTPHost, a.cfg.SMTPPort, auth, fromAddr.Address, a.cfg.Recipients, msg.Bytes())
	}
	if !a.cfg.SMTPInsecurePlain && a.cfg.SMTPPort == 25 {
		return errors.New("plain SMTP auth on port 25 is disabled; set SMTP_INSECURE_PLAIN_AUTH=true to override")
	}
	addr := net.JoinHostPort(a.cfg.SMTPHost, strconv.Itoa(a.cfg.SMTPPort))
	return smtp.SendMail(addr, auth, fromAddr.Address, a.cfg.Recipients, msg.Bytes())
}

func sendMailImplicitTLS(host string, port int, auth smtp.Auth, from string, recipients []string, msg []byte) error {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	if err != nil {
		return err
	}
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return err
	}
	defer client.Close()

	if err := client.Auth(auth); err != nil {
		return err
	}
	if err := client.Mail(from); err != nil {
		return err
	}
	for _, recipient := range recipients {
		if err := client.Rcpt(recipient); err != nil {
			return err
		}
	}
	writer, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := writer.Write(msg); err != nil {
		writer.Close()
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	return client.Quit()
}

func writeHeader(buf *bytes.Buffer, key, value string) {
	value = strings.ReplaceAll(value, "\r", "")
	value = strings.ReplaceAll(value, "\n", "")
	buf.WriteString(key)
	buf.WriteString(": ")
	buf.WriteString(value)
	buf.WriteString("\r\n")
}

func backoff(attempt int) time.Duration {
	seconds := math.Pow(2, float64(attempt)) * 30
	if seconds > 3600 {
		seconds = 3600
	}
	return time.Duration(seconds) * time.Second
}

func (a *App) cleanupOldSubmissions(ctx context.Context) {
	if a.cfg.RetentionDays <= 0 {
		return
	}
	cutoff := a.now().UTC().AddDate(0, 0, -a.cfg.RetentionDays).Format(time.RFC3339Nano)
	if _, err := a.db.ExecContext(ctx, `
		DELETE FROM submissions
		WHERE created_at < ?
		  AND status IN ('sent', 'dead')
	`, cutoff); err != nil {
		a.log.Warn("retention.cleanup_failed", "error", err)
	}
}

func (a *App) verifyTurnstile(ctx context.Context, token, remoteIP string) error {
	if token == "" {
		return errors.New("missing token")
	}
	form := url.Values{}
	form.Set("secret", a.cfg.TurnstileSecret)
	form.Set("response", token)
	if remoteIP != "" {
		form.Set("remoteip", remoteIP)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://challenges.cloudflare.com/turnstile/v0/siteverify", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var result struct {
		Success bool `json:"success"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return err
	}
	if !result.Success {
		return errors.New("turnstile rejected token")
	}
	return nil
}

func (a *App) originAllowed(origin string) bool {
	if origin == "" {
		return false
	}
	_, ok := a.originSet[origin]
	return ok
}

func setCORS(w http.ResponseWriter, origin string) {
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Vary", "Origin")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.Header().Set("Access-Control-Max-Age", "600")
}

func clientIP(r *http.Request) string {
	if forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); forwarded != "" {
		parts := strings.Split(forwarded, ",")
		return strings.TrimSpace(parts[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (a *App) hashIP(ip string) string {
	sum := sha256.Sum256([]byte(a.cfg.IPHashSecret + ":" + ip))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func successfulResponse(err error) sql.NullString {
	if err != nil {
		return sql.NullString{}
	}
	return sql.NullString{String: "accepted by smtp client", Valid: true}
}

func nullableString(value string) sql.NullString {
	if value == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: value, Valid: true}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func makeOriginSet(origins []string) map[string]struct{} {
	set := make(map[string]struct{}, len(origins))
	for _, origin := range origins {
		set[origin] = struct{}{}
	}
	return set
}

func splitCSV(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func env(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func envInt(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envBool(key string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func randomToken(bytesLen int) (string, error) {
	buf := make([]byte, bytesLen)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
