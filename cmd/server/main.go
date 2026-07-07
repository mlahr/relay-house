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
	"flag"
	"fmt"
	"io"
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
	"text/tabwriter"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
	_ "modernc.org/sqlite"
)

var version = "dev"

const packagedConfigPath = "/etc/relay-house/config.yaml"

const telegramAPIBase = "https://api.telegram.org"

type Config struct {
	Addr            string
	DatabasePath    string
	Providers       []ProviderConfig
	Projects        []ProjectConfig
	TurnstileSecret string
	RateMinute      int
	RateDay         int
	MaxRetries      int
	WorkerInterval  time.Duration
	RetentionDays   int
	IPHashSecret    string
}

type ProjectConfig struct {
	Key            string
	Name           string
	AllowedOrigins []string
	ProviderName   string
	originSet      map[string]struct{}
}

type ProviderConfig struct {
	Name              string
	Type              string
	From              string
	Recipients        []string
	SMTPHost          string
	SMTPPort          int
	SMTPUsername      string
	SMTPPassword      string
	SMTPInsecurePlain bool
	MailtrapAPIURL    string
	MailtrapAPIToken  string
	MailtrapBCC       []string
	TelegramBotToken  string
	TelegramChatIDs   []int64
	TelegramAPIBase   string
}

type App struct {
	cfg         Config
	db          *sql.DB
	log         *slog.Logger
	projectMap  map[string]ProjectConfig
	providerMap map[string]ProviderConfig
	now         func() time.Time
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
	Provider     ProviderConfig
	Name         string
	Email        string
	Subject      string
	Message      string
	AttemptCount int
}

type deliveryResult struct {
	Provider string
	Response string
	Err      error
}

type mailtrapAddress struct {
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
}

type telegramResponse struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Description string          `json:"description"`
}

type eventCommandOptions struct {
	ConfigPath   string
	DatabasePath string
	Limit        int
	JSON         bool
}

type eventRow struct {
	Time         string `json:"time"`
	Event        string `json:"event"`
	Status       string `json:"status"`
	Provider     string `json:"provider,omitempty"`
	SubmissionID string `json:"submission_id,omitempty"`
	JobID        string `json:"job_id,omitempty"`
	Attempt      int    `json:"attempt,omitempty"`
	Project      string `json:"project,omitempty"`
	Summary      string `json:"summary,omitempty"`
}

type fileConfig struct {
	HTTP struct {
		Address string `yaml:"address"`
	} `yaml:"http"`
	Database struct {
		Path string `yaml:"path"`
	} `yaml:"database"`
	Providers []fileProvider `yaml:"providers"`
	Projects  []fileProject  `yaml:"projects"`
	Turnstile struct {
		Secret string `yaml:"secret"`
	} `yaml:"turnstile"`
	RateLimit struct {
		PerMinute int `yaml:"per_minute"`
		PerDay    int `yaml:"per_day"`
	} `yaml:"rate_limit"`
	Worker struct {
		MaxRetries      int `yaml:"max_retries"`
		IntervalSeconds int `yaml:"interval_seconds"`
	} `yaml:"worker"`
	Retention struct {
		Days int `yaml:"days"`
	} `yaml:"retention"`
	Security struct {
		IPHashSecret string `yaml:"ip_hash_secret"`
	} `yaml:"security"`
}

type fileProject struct {
	Key            string   `yaml:"key"`
	Name           string   `yaml:"name"`
	AllowedOrigins []string `yaml:"allowed_origins"`
	ProviderName   string   `yaml:"provider"`
}

type fileProvider struct {
	Name              string   `yaml:"name"`
	Type              string   `yaml:"type"`
	From              string   `yaml:"from"`
	Recipients        []string `yaml:"recipients"`
	SMTPHost          string   `yaml:"host"`
	SMTPPort          int      `yaml:"port"`
	SMTPUsername      string   `yaml:"username"`
	SMTPPassword      string   `yaml:"password"`
	SMTPInsecurePlain bool     `yaml:"insecure_plain_auth"`
	MailtrapAPIURL    string   `yaml:"api_url"`
	MailtrapAPIToken  string   `yaml:"api_token"`
	MailtrapBCC       []string `yaml:"bcc"`
	TelegramBotToken  string   `yaml:"bot_token"`
	TelegramChatIDs   []int64  `yaml:"chat_ids"`
}

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "events" {
		if err := runEventsCommand(os.Stdout, args[1:]); err != nil {
			fmt.Fprintf(os.Stderr, "relay-house events: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		fmt.Fprintf(os.Stderr, "relay-house: unknown command %q\nRun 'relay-house --help' for usage.\n", args[0])
		os.Exit(1)
	}
	if hasHelpFlag(args) {
		printUsage(os.Stdout)
		return
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg, err := loadConfigFromArgs(args)
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
		cfg:         cfg,
		db:          db,
		log:         logger,
		projectMap:  makeProjectMap(cfg.Projects),
		providerMap: makeProviderMap(cfg.Providers),
		now:         time.Now,
	}

	if err := app.migrate(context.Background()); err != nil {
		logger.Error("db.migrate_failed", "error", err)
		os.Exit(1)
	}
	if err := app.seedProjects(context.Background()); err != nil {
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
	return loadConfigFromArgs(nil)
}

func hasHelpFlag(args []string) bool {
	for _, arg := range args {
		if arg == "-h" || arg == "--help" {
			return true
		}
	}
	return false
}

func printUsage(w io.Writer) {
	_, _ = fmt.Fprintf(w, `relay-house %s

Usage:
  relay-house [options]
  relay-house events [options]

Commands:
  events                  Print recent database events and exit.

Options:
  -config, --config PATH   Path to YAML config file.
  -h, --help              Print this help text.
  -version, --version     Print version.

Configuration:
  Values are loaded in this order:
    compiled defaults < YAML config from -config < environment variables

  Validation-required settings:
    projects[].key
    projects[].allowed_origins
    projects[].provider
    providers[].name
    providers[].type

  Recommended persistent settings:
    IP_HASH_SECRET

Examples:
  relay-house -config /etc/relay-house/config.yaml
  relay-house events -config /etc/relay-house/config.yaml -limit 50
  relay-house -version
`, version)
}

func printEventsUsage(w io.Writer) {
	_, _ = fmt.Fprint(w, `Usage:
  relay-house events [options]

Options:
  -config, --config PATH     Path to YAML config file.
  -database, --database PATH SQLite database path override.
  -limit N                  Maximum events to print. Default 25, maximum 500.
  --json                    Print a JSON array instead of a table.
  -h, --help                Print this help text.
`)
}

func runEventsCommand(w io.Writer, args []string) error {
	if hasHelpFlag(args) {
		printEventsUsage(w)
		return nil
	}
	opts, err := parseEventCommandOptions(args)
	if err != nil {
		return err
	}
	events, err := loadEvents(opts)
	if err != nil {
		return err
	}
	if opts.JSON {
		return writeEventsJSON(w, events)
	}
	return writeEventsTable(w, events)
}

func parseEventCommandOptions(args []string) (eventCommandOptions, error) {
	opts := eventCommandOptions{Limit: 25}
	fs := flag.NewFlagSet("relay-house events", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&opts.ConfigPath, "config", "", "path to YAML config file")
	fs.StringVar(&opts.DatabasePath, "database", "", "SQLite database path override")
	fs.IntVar(&opts.Limit, "limit", opts.Limit, "maximum events to print")
	fs.BoolVar(&opts.JSON, "json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return eventCommandOptions{}, err
	}
	if fs.NArg() > 0 {
		return eventCommandOptions{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if opts.Limit < 1 {
		return eventCommandOptions{}, errors.New("-limit must be at least 1")
	}
	if opts.Limit > 500 {
		return eventCommandOptions{}, errors.New("-limit must be at most 500")
	}
	databasePath, err := eventDatabasePath(opts.ConfigPath, opts.DatabasePath)
	if err != nil {
		return eventCommandOptions{}, err
	}
	opts.DatabasePath = databasePath
	return opts, nil
}

func eventDatabasePath(configPath, databaseOverride string) (string, error) {
	cfg := defaultConfig()
	if configPath == "" {
		configPath = os.Getenv("RELAY_HOUSE_CONFIG")
	}
	if configPath == "" && fileExists(packagedConfigPath) {
		configPath = packagedConfigPath
	}
	if configPath != "" {
		if err := applyYAMLConfig(&cfg, configPath); err != nil {
			return "", err
		}
	}
	setStringFromEnv(&cfg.DatabasePath, "DATABASE_PATH")
	setString(&cfg.DatabasePath, databaseOverride)
	return cfg.DatabasePath, nil
}

func loadEvents(opts eventCommandOptions) ([]eventRow, error) {
	if !fileExists(opts.DatabasePath) {
		return nil, fmt.Errorf("database file %q does not exist; pass -config or -database", opts.DatabasePath)
	}
	db, err := sql.Open("sqlite", readOnlySQLiteDSN(opts.DatabasePath))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	return queryEvents(context.Background(), db, opts.Limit)
}

func readOnlySQLiteDSN(path string) string {
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Set("mode", "ro")
	q.Set("_pragma", "busy_timeout(5000)")
	u.RawQuery = q.Encode()
	return u.String()
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func queryEvents(ctx context.Context, db *sql.DB, limit int) ([]eventRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT event_time, event, status, provider, submission_id, job_id, attempt, project, summary
		FROM (
			SELECT
				s.created_at AS event_time,
				'submission.accepted' AS event,
				s.status AS status,
				'' AS provider,
				s.id AS submission_id,
				'' AS job_id,
				0 AS attempt,
				s.project_key AS project,
				'origin=' || s.origin AS summary
			FROM submissions s
			UNION ALL
			SELECT
				a.created_at AS event_time,
				CASE
					WHEN a.status = 'sent' THEN 'delivery.attempt.sent'
					ELSE 'delivery.attempt.failed'
				END AS event,
				a.status AS status,
				a.provider AS provider,
				a.submission_id AS submission_id,
				a.job_id AS job_id,
				a.attempt_number AS attempt,
				s.project_key AS project,
				COALESCE(a.error, '')
			FROM delivery_attempts a
			JOIN submissions s ON s.id = a.submission_id
			UNION ALL
			SELECT
				j.updated_at AS event_time,
				'delivery.job.current' AS event,
				j.status AS status,
				'' AS provider,
				j.submission_id AS submission_id,
				j.id AS job_id,
				j.attempt_count AS attempt,
				s.project_key AS project,
				CASE
					WHEN j.last_error IS NOT NULL AND j.last_error != '' THEN j.last_error
					WHEN j.status = 'retry' THEN 'next_attempt_at=' || j.next_attempt_at
					ELSE ''
				END AS summary
			FROM delivery_jobs j
			JOIN submissions s ON s.id = j.submission_id
			WHERE j.status IN ('queued', 'retry', 'sending', 'dead')
		)
		ORDER BY event_time DESC, event DESC, submission_id DESC, job_id DESC, attempt DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	events := make([]eventRow, 0, limit)
	for rows.Next() {
		var event eventRow
		if err := rows.Scan(&event.Time, &event.Event, &event.Status, &event.Provider, &event.SubmissionID, &event.JobID, &event.Attempt, &event.Project, &event.Summary); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return events, nil
}

func writeEventsJSON(w io.Writer, events []eventRow) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(events)
}

func writeEventsTable(w io.Writer, events []eventRow) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "time\tevent\tstatus\tprovider\tsubmission_id\tjob_id\tattempt\tproject\tsummary"); err != nil {
		return err
	}
	for _, event := range events {
		attempt := ""
		if event.Attempt > 0 {
			attempt = strconv.Itoa(event.Attempt)
		}
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			event.Time,
			event.Event,
			event.Status,
			event.Provider,
			event.SubmissionID,
			event.JobID,
			attempt,
			event.Project,
			event.Summary,
		); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func loadConfigFromArgs(args []string) (Config, error) {
	fs := flag.NewFlagSet("relay-house", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configPath := fs.String("config", "", "path to YAML config file")
	showVersion := fs.Bool("version", false, "print version")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if *showVersion {
		fmt.Printf("relay-house %s\n", version)
		os.Exit(0)
	}

	cfg := defaultConfig()
	if *configPath != "" {
		if err := applyYAMLConfig(&cfg, *configPath); err != nil {
			return Config{}, err
		}
	}
	if err := applyEnvConfig(&cfg); err != nil {
		return Config{}, err
	}
	return validateConfig(cfg)
}

func defaultConfig() Config {
	return Config{
		Addr:           ":8080",
		DatabasePath:   "relay-house.db",
		RateMinute:     5,
		RateDay:        100,
		MaxRetries:     5,
		WorkerInterval: 5 * time.Second,
		RetentionDays:  365,
	}
}

func applyYAMLConfig(cfg *Config, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var file fileConfig
	if err := yaml.Unmarshal(data, &file); err != nil {
		return err
	}

	setString(&cfg.Addr, file.HTTP.Address)
	setString(&cfg.DatabasePath, file.Database.Path)
	if len(file.Providers) > 0 {
		providers := make([]ProviderConfig, 0, len(file.Providers))
		for _, item := range file.Providers {
			provider := ProviderConfig{
				Name:              strings.TrimSpace(item.Name),
				Type:              strings.ToLower(strings.TrimSpace(item.Type)),
				From:              strings.TrimSpace(item.From),
				Recipients:        trimStrings(item.Recipients),
				SMTPHost:          strings.TrimSpace(item.SMTPHost),
				SMTPPort:          item.SMTPPort,
				SMTPUsername:      strings.TrimSpace(item.SMTPUsername),
				SMTPPassword:      strings.TrimSpace(item.SMTPPassword),
				SMTPInsecurePlain: item.SMTPInsecurePlain,
				MailtrapAPIURL:    strings.TrimSpace(item.MailtrapAPIURL),
				MailtrapAPIToken:  strings.TrimSpace(item.MailtrapAPIToken),
				MailtrapBCC:       trimStrings(item.MailtrapBCC),
				TelegramBotToken:  strings.TrimSpace(item.TelegramBotToken),
				TelegramChatIDs:   append([]int64(nil), item.TelegramChatIDs...),
				TelegramAPIBase:   telegramAPIBase,
			}
			providers = append(providers, provider)
		}
		cfg.Providers = providers
	}
	if len(file.Projects) > 0 {
		projects := make([]ProjectConfig, 0, len(file.Projects))
		for _, item := range file.Projects {
			project := ProjectConfig{
				Key:            strings.TrimSpace(item.Key),
				Name:           strings.TrimSpace(item.Name),
				AllowedOrigins: trimStrings(item.AllowedOrigins),
				ProviderName:   strings.TrimSpace(item.ProviderName),
			}
			projects = append(projects, project)
		}
		cfg.Projects = projects
	}
	setString(&cfg.TurnstileSecret, file.Turnstile.Secret)
	setInt(&cfg.RateMinute, file.RateLimit.PerMinute)
	setInt(&cfg.RateDay, file.RateLimit.PerDay)
	setInt(&cfg.MaxRetries, file.Worker.MaxRetries)
	if file.Worker.IntervalSeconds > 0 {
		cfg.WorkerInterval = time.Duration(file.Worker.IntervalSeconds) * time.Second
	}
	setInt(&cfg.RetentionDays, file.Retention.Days)
	setString(&cfg.IPHashSecret, file.Security.IPHashSecret)
	return nil
}

func applyEnvConfig(cfg *Config) error {
	setStringFromEnv(&cfg.Addr, "ADDR")
	setStringFromEnv(&cfg.DatabasePath, "DATABASE_PATH")
	setStringFromEnv(&cfg.TurnstileSecret, "TURNSTILE_SECRET")
	if err := setIntFromEnv(&cfg.RateMinute, "RATE_LIMIT_PER_MINUTE"); err != nil {
		return err
	}
	if err := setIntFromEnv(&cfg.RateDay, "RATE_LIMIT_PER_DAY"); err != nil {
		return err
	}
	if err := setIntFromEnv(&cfg.MaxRetries, "MAX_RETRIES"); err != nil {
		return err
	}
	if value, ok := os.LookupEnv("WORKER_INTERVAL_SECONDS"); ok {
		seconds, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("WORKER_INTERVAL_SECONDS must be an integer: %w", err)
		}
		cfg.WorkerInterval = time.Duration(seconds) * time.Second
	}
	if err := setIntFromEnv(&cfg.RetentionDays, "RETENTION_DAYS"); err != nil {
		return err
	}
	setStringFromEnv(&cfg.IPHashSecret, "IP_HASH_SECRET")
	return nil
}

func validateConfig(cfg Config) (Config, error) {
	if len(cfg.Providers) == 0 {
		return Config{}, errors.New("at least one provider is required")
	}
	providerMap := make(map[string]ProviderConfig, len(cfg.Providers))
	for i := range cfg.Providers {
		provider := &cfg.Providers[i]
		provider.Name = strings.TrimSpace(provider.Name)
		provider.Type = strings.ToLower(strings.TrimSpace(provider.Type))
		provider.From = strings.TrimSpace(provider.From)
		provider.Recipients = trimStrings(provider.Recipients)
		provider.SMTPHost = strings.TrimSpace(provider.SMTPHost)
		provider.SMTPUsername = strings.TrimSpace(provider.SMTPUsername)
		provider.SMTPPassword = strings.TrimSpace(provider.SMTPPassword)
		provider.MailtrapAPIURL = strings.TrimSpace(provider.MailtrapAPIURL)
		provider.MailtrapAPIToken = strings.TrimSpace(provider.MailtrapAPIToken)
		provider.MailtrapBCC = trimStrings(provider.MailtrapBCC)
		provider.TelegramBotToken = strings.TrimSpace(provider.TelegramBotToken)
		if provider.Name == "" {
			return Config{}, fmt.Errorf("providers[%d].name is required", i)
		}
		if _, ok := providerMap[provider.Name]; ok {
			return Config{}, fmt.Errorf("duplicate provider name %q", provider.Name)
		}
		switch provider.Type {
		case "smtp":
			if provider.From == "" {
				return Config{}, fmt.Errorf("providers[%d].from is required when type=smtp", i)
			}
			if _, err := mail.ParseAddress(provider.From); err != nil {
				return Config{}, fmt.Errorf("providers[%d].from is invalid: %w", i, err)
			}
			if len(provider.Recipients) == 0 {
				return Config{}, fmt.Errorf("providers[%d].recipients is required when type=smtp", i)
			}
			if _, err := envelopeAddresses(provider.Recipients); err != nil {
				return Config{}, fmt.Errorf("providers[%d].recipients is invalid: %w", i, err)
			}
			if provider.SMTPHost == "" {
				return Config{}, fmt.Errorf("providers[%d].host is required when type=smtp", i)
			}
			if provider.SMTPPort == 0 {
				provider.SMTPPort = 587
			}
			if provider.SMTPUsername == "" || provider.SMTPPassword == "" {
				return Config{}, fmt.Errorf("providers[%d].username and password are required when type=smtp", i)
			}
		case "mailtrap":
			if provider.From == "" {
				return Config{}, fmt.Errorf("providers[%d].from is required when type=mailtrap", i)
			}
			if _, err := mail.ParseAddress(provider.From); err != nil {
				return Config{}, fmt.Errorf("providers[%d].from is invalid: %w", i, err)
			}
			if len(provider.Recipients) == 0 {
				return Config{}, fmt.Errorf("providers[%d].recipients is required when type=mailtrap", i)
			}
			if _, err := envelopeAddresses(provider.Recipients); err != nil {
				return Config{}, fmt.Errorf("providers[%d].recipients is invalid: %w", i, err)
			}
			if provider.MailtrapAPIToken == "" {
				return Config{}, fmt.Errorf("providers[%d].api_token is required when type=mailtrap", i)
			}
			if provider.MailtrapAPIURL == "" {
				provider.MailtrapAPIURL = "https://send.api.mailtrap.io/api/send"
			}
		case "telegram":
			if provider.TelegramBotToken == "" {
				return Config{}, fmt.Errorf("providers[%d].bot_token is required when type=telegram", i)
			}
			if len(provider.TelegramChatIDs) == 0 {
				return Config{}, fmt.Errorf("providers[%d].chat_ids is required when type=telegram", i)
			}
			for j, chatID := range provider.TelegramChatIDs {
				if chatID == 0 {
					return Config{}, fmt.Errorf("providers[%d].chat_ids[%d] must not be zero", i, j)
				}
			}
			if strings.TrimSpace(provider.TelegramAPIBase) == "" {
				provider.TelegramAPIBase = telegramAPIBase
			}
		default:
			return Config{}, fmt.Errorf("providers[%d].type %q is unsupported", i, provider.Type)
		}
		providerMap[provider.Name] = *provider
	}
	if len(cfg.Projects) == 0 {
		return Config{}, errors.New("at least one project is required")
	}
	seen := make(map[string]struct{}, len(cfg.Projects))
	for i := range cfg.Projects {
		project := &cfg.Projects[i]
		project.Key = strings.TrimSpace(project.Key)
		project.Name = strings.TrimSpace(project.Name)
		project.AllowedOrigins = trimStrings(project.AllowedOrigins)
		project.ProviderName = strings.TrimSpace(project.ProviderName)
		if project.Key == "" {
			return Config{}, fmt.Errorf("projects[%d].key is required", i)
		}
		if _, ok := seen[project.Key]; ok {
			return Config{}, fmt.Errorf("duplicate project key %q", project.Key)
		}
		seen[project.Key] = struct{}{}
		if project.Name == "" {
			project.Name = project.Key
		}
		if len(project.AllowedOrigins) == 0 {
			return Config{}, fmt.Errorf("projects[%d].allowed_origins is required", i)
		}
		if project.ProviderName == "" {
			return Config{}, fmt.Errorf("projects[%d].provider is required", i)
		}
		if _, ok := providerMap[project.ProviderName]; !ok {
			return Config{}, fmt.Errorf("projects[%d].provider %q is not defined", i, project.ProviderName)
		}
		project.originSet = makeOriginSet(project.AllowedOrigins)
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
	if a.originAllowedByAnyProject(origin) {
		setCORS(w, origin)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) send(w http.ResponseWriter, r *http.Request) {
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
	project, ok := a.projectMap[req.Project]
	if !ok {
		writeJSON(w, http.StatusForbidden, SendResponse{Status: "rejected", Error: "unknown project"})
		return
	}
	origin := r.Header.Get("Origin")
	if !project.originAllowed(origin) {
		a.log.Warn("request.origin_rejected", "project", req.Project, "origin", origin)
		writeJSON(w, http.StatusForbidden, SendResponse{Status: "rejected", Error: "origin not allowed"})
		return
	}
	setCORS(w, origin)

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
			from_address TEXT NOT NULL DEFAULT '',
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
	if err := a.ensureProjectFromColumn(ctx); err != nil {
		return err
	}
	return nil
}

func (a *App) ensureProjectFromColumn(ctx context.Context) error {
	hasColumn, err := tableHasColumn(ctx, a.db, "projects", "from_address")
	if err != nil {
		return err
	}
	if hasColumn {
		return nil
	}
	_, err = a.db.ExecContext(ctx, `ALTER TABLE projects ADD COLUMN from_address TEXT NOT NULL DEFAULT ''`)
	return err
}

func tableHasColumn(ctx context.Context, db *sql.DB, table, column string) (bool, error) {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name string
		var typ string
		var notNull int
		var defaultValue sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (a *App) seedProjects(ctx context.Context) error {
	now := a.now().UTC().Format(time.RFC3339Nano)
	for _, project := range a.cfg.Projects {
		origins, err := json.Marshal(project.AllowedOrigins)
		if err != nil {
			return err
		}
		_, err = a.db.ExecContext(ctx, `
			INSERT INTO projects (key, name, from_address, recipients_json, allowed_origins_json, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET
				name = excluded.name,
				from_address = excluded.from_address,
				recipients_json = excluded.recipients_json,
				allowed_origins_json = excluded.allowed_origins_json,
				updated_at = excluded.updated_at
		`, project.Key, project.Name, "", "[]", string(origins), now, now)
		if err != nil {
			return err
		}
	}
	return nil
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
	project, ok := a.projectMap[j.ProjectKey]
	if !ok {
		return job{}, false, fmt.Errorf("project %q is not configured", j.ProjectKey)
	}
	provider, ok := a.providerMap[project.ProviderName]
	if !ok {
		return job{}, false, fmt.Errorf("provider %q for project %q is not configured", project.ProviderName, j.ProjectKey)
	}
	j.Provider = provider

	if err := tx.Commit(); err != nil {
		return job{}, false, err
	}
	return j, true, nil
}

func (a *App) deliverJob(ctx context.Context, j job) {
	start := a.now()
	attemptNumber := j.AttemptCount + 1
	result := a.deliver(ctx, j)
	duration := time.Since(start).Milliseconds()

	now := a.now().UTC().Format(time.RFC3339Nano)
	status := "sent"
	errorText := ""
	if result.Err != nil {
		status = "failed"
		errorText = result.Err.Error()
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
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, uuid.NewString(), j.ID, j.SubmissionID, attemptNumber, status, result.Provider, nullableString(result.Response), nullableString(errorText), duration, now)
	if txErr != nil {
		a.log.Error("attempt.store_failed", "job_id", j.ID, "error", txErr)
		return
	}

	if result.Err == nil {
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

	if result.Err != nil {
		a.log.Warn("delivery.failed", "provider", result.Provider, "job_id", j.ID, "submission_id", j.SubmissionID, "attempt", attemptNumber, "error", result.Err)
		return
	}
	a.log.Info("delivery.sent", "provider", result.Provider, "job_id", j.ID, "submission_id", j.SubmissionID, "duration_ms", duration)
}

func (a *App) deliver(ctx context.Context, j job) deliveryResult {
	switch j.Provider.Type {
	case "smtp":
		err := a.sendSMTP(j, j.Provider)
		if err != nil {
			return deliveryResult{Provider: j.Provider.Name, Err: err}
		}
		return deliveryResult{Provider: j.Provider.Name, Response: "accepted by smtp client"}
	case "mailtrap":
		response, err := a.sendMailtrap(ctx, j, j.Provider)
		return deliveryResult{Provider: j.Provider.Name, Response: response, Err: err}
	case "telegram":
		response, err := a.sendTelegram(ctx, j, j.Provider)
		return deliveryResult{Provider: j.Provider.Name, Response: response, Err: err}
	default:
		return deliveryResult{Provider: j.Provider.Name, Err: fmt.Errorf("unsupported provider type %q", j.Provider.Type)}
	}
}

func (a *App) sendSMTP(j job, provider ProviderConfig) error {
	fromAddr, err := mail.ParseAddress(provider.From)
	if err != nil {
		return fmt.Errorf("invalid project from: %w", err)
	}
	replyTo, err := mail.ParseAddress(fmt.Sprintf("%s <%s>", j.Name, j.Email))
	if err != nil {
		return fmt.Errorf("invalid reply-to: %w", err)
	}

	var msg bytes.Buffer
	writeHeader(&msg, "From", fromAddr.String())
	writeHeader(&msg, "To", strings.Join(provider.Recipients, ", "))
	writeHeader(&msg, "Reply-To", replyTo.String())
	writeHeader(&msg, "Subject", j.Subject)
	writeHeader(&msg, "MIME-Version", "1.0")
	writeHeader(&msg, "Content-Type", `text/plain; charset="utf-8"`)
	writeHeader(&msg, "Content-Transfer-Encoding", "8bit")
	msg.WriteString("\r\n")
	msg.WriteString(j.Message)
	msg.WriteString("\r\n")

	auth := smtp.PlainAuth("", provider.SMTPUsername, provider.SMTPPassword, provider.SMTPHost)
	envelopeRecipients, err := envelopeAddresses(provider.Recipients)
	if err != nil {
		return err
	}
	if provider.SMTPPort == 465 {
		return sendMailImplicitTLS(provider.SMTPHost, provider.SMTPPort, auth, fromAddr.Address, envelopeRecipients, msg.Bytes())
	}
	if !provider.SMTPInsecurePlain && provider.SMTPPort == 25 {
		return errors.New("plain SMTP auth on port 25 is disabled; set insecure_plain_auth: true to override")
	}
	addr := net.JoinHostPort(provider.SMTPHost, strconv.Itoa(provider.SMTPPort))
	return smtp.SendMail(addr, auth, fromAddr.Address, envelopeRecipients, msg.Bytes())
}

func (a *App) sendMailtrap(ctx context.Context, j job, provider ProviderConfig) (string, error) {
	fromAddr, err := mail.ParseAddress(provider.From)
	if err != nil {
		return "", fmt.Errorf("invalid project from: %w", err)
	}
	replyTo, err := mail.ParseAddress(fmt.Sprintf("%s <%s>", j.Name, j.Email))
	if err != nil {
		return "", fmt.Errorf("invalid reply-to: %w", err)
	}

	type payload struct {
		From    mailtrapAddress   `json:"from"`
		To      []mailtrapAddress `json:"to"`
		BCC     []mailtrapAddress `json:"bcc,omitempty"`
		ReplyTo mailtrapAddress   `json:"reply_to"`
		Subject string            `json:"subject"`
		Text    string            `json:"text"`
		HTML    string            `json:"html,omitempty"`
	}

	body := payload{
		From:    mailtrapAddress{Email: fromAddr.Address, Name: fromAddr.Name},
		To:      addressesFromEmails(provider.Recipients),
		BCC:     addressesFromEmails(provider.MailtrapBCC),
		ReplyTo: mailtrapAddress{Email: replyTo.Address, Name: replyTo.Name},
		Subject: j.Subject,
		Text:    j.Message,
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, provider.MailtrapAPIURL, bytes.NewReader(encoded))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+provider.MailtrapAPIToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	responseBytes, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", err
	}
	responseText := strings.TrimSpace(string(responseBytes))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if responseText == "" {
			responseText = resp.Status
		}
		return responseText, fmt.Errorf("mailtrap returned %s", resp.Status)
	}
	if responseText == "" {
		responseText = resp.Status
	}
	return responseText, nil
}

func (a *App) sendTelegram(ctx context.Context, j job, provider ProviderConfig) (string, error) {
	type payload struct {
		ChatID int64  `json:"chat_id"`
		Text   string `json:"text"`
	}

	client := &http.Client{Timeout: 15 * time.Second}
	message := formatTelegramMessage(j)
	endpoint := fmt.Sprintf("%s/bot%s/sendMessage", strings.TrimRight(provider.TelegramAPIBase, "/"), provider.TelegramBotToken)
	for _, chatID := range provider.TelegramChatIDs {
		body, err := json.Marshal(payload{ChatID: chatID, Text: message})
		if err != nil {
			return "", err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			return "", err
		}
		responseText, readErr := readTelegramResponse(resp)
		resp.Body.Close()
		if readErr != nil {
			return responseText, readErr
		}
	}
	return fmt.Sprintf("sent to %d telegram chat(s)", len(provider.TelegramChatIDs)), nil
}

func readTelegramResponse(resp *http.Response) (string, error) {
	responseBytes, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", err
	}
	responseText := strings.TrimSpace(string(responseBytes))
	if responseText == "" {
		responseText = resp.Status
	}

	var result telegramResponse
	if err := json.Unmarshal(responseBytes, &result); err != nil {
		return responseText, fmt.Errorf("failed to decode Telegram response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || !result.OK {
		description := result.Description
		if description == "" {
			description = resp.Status
		}
		return responseText, fmt.Errorf("telegram returned %s", description)
	}
	return responseText, nil
}

func formatTelegramMessage(j job) string {
	var msg strings.Builder
	fmt.Fprintf(&msg, "New RelayHouse submission\n")
	fmt.Fprintf(&msg, "Project: %s\n", j.ProjectKey)
	fmt.Fprintf(&msg, "From: %s <%s>\n", j.Name, j.Email)
	fmt.Fprintf(&msg, "Subject: %s\n\n", j.Subject)
	msg.WriteString(j.Message)
	return msg.String()
}

func addressesFromEmails(values []string) []mailtrapAddress {
	addresses := make([]mailtrapAddress, 0, len(values))
	for _, value := range values {
		parsed, err := mail.ParseAddress(value)
		if err == nil {
			addresses = append(addresses, mailtrapAddress{Email: parsed.Address, Name: parsed.Name})
			continue
		}
		addresses = append(addresses, mailtrapAddress{Email: value})
	}
	return addresses
}

func envelopeAddresses(values []string) ([]string, error) {
	addresses := make([]string, 0, len(values))
	for _, value := range values {
		parsed, err := mail.ParseAddress(value)
		if err != nil {
			return nil, fmt.Errorf("invalid recipient %q: %w", value, err)
		}
		addresses = append(addresses, parsed.Address)
	}
	return addresses, nil
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

func (a *App) originAllowedByAnyProject(origin string) bool {
	if origin == "" {
		return false
	}
	for _, project := range a.projectMap {
		if project.originAllowed(origin) {
			return true
		}
	}
	return false
}

func (p ProjectConfig) originAllowed(origin string) bool {
	if origin == "" {
		return false
	}
	_, ok := p.originSet[origin]
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
		origin = strings.TrimSpace(origin)
		if origin != "" {
			set[origin] = struct{}{}
		}
	}
	return set
}

func makeProjectMap(projects []ProjectConfig) map[string]ProjectConfig {
	projectMap := make(map[string]ProjectConfig, len(projects))
	for _, project := range projects {
		if project.originSet == nil {
			project.originSet = makeOriginSet(project.AllowedOrigins)
		}
		projectMap[project.Key] = project
	}
	return projectMap
}

func makeProviderMap(providers []ProviderConfig) map[string]ProviderConfig {
	providerMap := make(map[string]ProviderConfig, len(providers))
	for _, provider := range providers {
		providerMap[provider.Name] = provider
	}
	return providerMap
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

func trimStrings(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

func randomToken(bytesLen int) (string, error) {
	buf := make([]byte, bytesLen)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func setString(dst *string, value string) {
	if strings.TrimSpace(value) != "" {
		*dst = strings.TrimSpace(value)
	}
}

func setStrings(dst *[]string, values []string) {
	if len(values) > 0 {
		*dst = values
	}
}

func setInt(dst *int, value int) {
	if value > 0 {
		*dst = value
	}
}

func setStringFromEnv(dst *string, key string) {
	if value, ok := os.LookupEnv(key); ok {
		*dst = strings.TrimSpace(value)
	}
}

func setLowerStringFromEnv(dst *string, key string) {
	if value, ok := os.LookupEnv(key); ok {
		*dst = strings.ToLower(strings.TrimSpace(value))
	}
}

func setCSVFromEnv(dst *[]string, key string) {
	if value, ok := os.LookupEnv(key); ok {
		*dst = splitCSV(value)
	}
}

func setIntFromEnv(dst *int, key string) error {
	value, ok := os.LookupEnv(key)
	if !ok {
		return nil
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return fmt.Errorf("%s must be an integer: %w", key, err)
	}
	*dst = parsed
	return nil
}

func setBoolFromEnv(dst *bool, key string) error {
	value, ok := os.LookupEnv(key)
	if !ok {
		return nil
	}
	parsed, err := strconv.ParseBool(strings.TrimSpace(value))
	if err != nil {
		return fmt.Errorf("%s must be a boolean: %w", key, err)
	}
	*dst = parsed
	return nil
}
