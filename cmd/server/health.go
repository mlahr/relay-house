package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/smtp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	healthCheckTimeout = 5 * time.Second
	healthCacheTTL     = 60 * time.Second
)

const (
	healthOK        = "ok"
	healthUnhealthy = "unhealthy"
	healthUnknown   = "unknown"

	liveCheckOK           = "ok"
	liveCheckFailed       = "failed"
	liveCheckNotPerformed = "not_performed"

	lastAttemptSucceeded = "succeeded"
	lastAttemptFailed    = "failed"
	lastAttemptNone      = "none"
	lastAttemptUnknown   = "unknown"
)

const (
	reasonDatabaseUnavailable  = "database_unavailable"
	reasonHistoryUnavailable   = "history_unavailable"
	reasonConnectivityFailed   = "connectivity_failed"
	reasonAuthenticationFailed = "authentication_failed"
	reasonProbeTimeout         = "probe_timeout"
	reasonUnexpectedResponse   = "unexpected_response"
	reasonLatestAttemptFailed  = "latest_attempt_failed"
)

type healthResponse struct {
	Status    string                   `json:"status"`
	Error     string                   `json:"error,omitempty"`
	Database  healthDatabase           `json:"database"`
	Providers []healthProviderResponse `json:"providers"`
	CheckedAt string                   `json:"checked_at"`
}

type healthDatabase struct {
	Status      string   `json:"status"`
	ReasonCodes []string `json:"reason_codes,omitempty"`
}

type healthProviderResponse struct {
	Name          string   `json:"name"`
	Type          string   `json:"type"`
	Status        string   `json:"status"`
	LiveCheck     string   `json:"live_check"`
	LiveCheckedAt string   `json:"live_checked_at,omitempty"`
	LastAttempt   string   `json:"last_attempt"`
	LastAttemptAt string   `json:"last_attempt_at,omitempty"`
	ReasonCodes   []string `json:"reason_codes,omitempty"`
}

type latestDeliveryAttempt struct {
	Status    string
	CreatedAt string
}

type providerLiveHealth struct {
	Status      string
	CheckedAt   string
	ReasonCodes []string
}

type providerHealthCache struct {
	ExpiresAt time.Time
	Results   map[string]providerLiveHealth
}

func (a *App) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), healthCheckTimeout)
	defer cancel()

	checkedAt := a.currentTime().UTC()
	database, attempts := a.deliveryHistoryHealth(ctx)
	liveResults := a.cachedProviderHealth(ctx)

	response := healthResponse{
		Status:    healthOK,
		Database:  database,
		Providers: make([]healthProviderResponse, 0, len(a.cfg.Providers)),
		CheckedAt: checkedAt.Format(time.RFC3339Nano),
	}
	if database.Status != healthOK {
		response.Status = healthUnhealthy
		response.Error = "database unavailable"
	}

	for _, provider := range a.cfg.Providers {
		result := healthProviderResponse{
			Name:        provider.Name,
			Type:        provider.Type,
			Status:      healthOK,
			LiveCheck:   liveCheckNotPerformed,
			LastAttempt: lastAttemptNone,
		}

		if provider.Type != "mailtrap" {
			live, ok := liveResults[provider.Name]
			if !ok {
				live = providerLiveHealth{Status: liveCheckFailed, ReasonCodes: []string{reasonUnexpectedResponse}}
			}
			result.LiveCheck = live.Status
			result.LiveCheckedAt = live.CheckedAt
			result.ReasonCodes = append(result.ReasonCodes, live.ReasonCodes...)
			if live.Status != liveCheckOK {
				result.Status = healthUnhealthy
			}
		}

		if database.Status != healthOK {
			result.LastAttempt = lastAttemptUnknown
			result.ReasonCodes = appendReason(result.ReasonCodes, reasonHistoryUnavailable)
			if result.Status == healthOK {
				result.Status = healthUnknown
			}
		} else if attempt, ok := attempts[provider.Name]; ok {
			result.LastAttemptAt = attempt.CreatedAt
			switch attempt.Status {
			case "sent":
				result.LastAttempt = lastAttemptSucceeded
			case "failed":
				result.LastAttempt = lastAttemptFailed
				result.Status = healthUnhealthy
				result.ReasonCodes = appendReason(result.ReasonCodes, reasonLatestAttemptFailed)
			default:
				result.LastAttempt = lastAttemptUnknown
				result.Status = healthUnhealthy
				result.ReasonCodes = appendReason(result.ReasonCodes, reasonUnexpectedResponse)
			}
		}

		if result.Status != healthOK {
			response.Status = healthUnhealthy
			if response.Error == "" {
				response.Error = "delivery provider unavailable"
			}
		}
		response.Providers = append(response.Providers, result)
	}

	status := http.StatusOK
	if response.Status != healthOK {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, response)
}

func (a *App) deliveryHistoryHealth(ctx context.Context) (healthDatabase, map[string]latestDeliveryAttempt) {
	if err := a.db.PingContext(ctx); err != nil {
		return healthDatabase{Status: healthUnhealthy, ReasonCodes: []string{reasonDatabaseUnavailable}}, nil
	}

	rows, err := a.db.QueryContext(ctx, `
		SELECT provider, status, created_at
		FROM (
			SELECT
				provider,
				status,
				created_at,
				ROW_NUMBER() OVER (
					PARTITION BY provider
					ORDER BY created_at DESC, rowid DESC
				) AS position
			FROM delivery_attempts
		)
		WHERE position = 1
	`)
	if err != nil {
		return healthDatabase{Status: healthUnhealthy, ReasonCodes: []string{reasonHistoryUnavailable}}, nil
	}
	defer rows.Close()

	attempts := make(map[string]latestDeliveryAttempt)
	for rows.Next() {
		var provider string
		var attempt latestDeliveryAttempt
		if err := rows.Scan(&provider, &attempt.Status, &attempt.CreatedAt); err != nil {
			return healthDatabase{Status: healthUnhealthy, ReasonCodes: []string{reasonHistoryUnavailable}}, nil
		}
		attempts[provider] = attempt
	}
	if err := rows.Err(); err != nil {
		return healthDatabase{Status: healthUnhealthy, ReasonCodes: []string{reasonHistoryUnavailable}}, nil
	}
	return healthDatabase{Status: healthOK}, attempts
}

func (a *App) cachedProviderHealth(ctx context.Context) map[string]providerLiveHealth {
	a.healthMu.Lock()
	defer a.healthMu.Unlock()

	now := a.currentTime().UTC()
	if a.healthCache.Results != nil && now.Before(a.healthCache.ExpiresAt) {
		return cloneProviderHealth(a.healthCache.Results)
	}

	results := a.probeProviders(ctx, now)
	a.healthCache = providerHealthCache{
		ExpiresAt: now.Add(healthCacheTTL),
		Results:   cloneProviderHealth(results),
	}
	return results
}

func (a *App) probeProviders(ctx context.Context, checkedAt time.Time) map[string]providerLiveHealth {
	results := make(map[string]providerLiveHealth)
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, configured := range a.cfg.Providers {
		if configured.Type == "mailtrap" {
			continue
		}
		provider := configured
		wg.Add(1)
		go func() {
			defer wg.Done()
			probe := a.probeHealth
			if probe == nil {
				probe = probeProviderHealth
			}
			result := probe(ctx, provider)
			result.CheckedAt = checkedAt.Format(time.RFC3339Nano)
			mu.Lock()
			results[provider.Name] = result
			mu.Unlock()
		}()
	}

	wg.Wait()
	return results
}

func probeProviderHealth(ctx context.Context, provider ProviderConfig) providerLiveHealth {
	switch provider.Type {
	case "smtp":
		return probeSMTPHealth(ctx, provider)
	case "telegram":
		return probeTelegramHealth(ctx, provider)
	default:
		return failedLiveHealth(reasonUnexpectedResponse)
	}
}

func probeSMTPHealth(ctx context.Context, provider ProviderConfig) providerLiveHealth {
	addr := net.JoinHostPort(provider.SMTPHost, strconv.Itoa(provider.SMTPPort))
	deadline := time.Now().Add(healthCheckTimeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}

	var conn net.Conn
	var err error
	if provider.SMTPPort == 465 {
		dialer := tls.Dialer{
			NetDialer: &net.Dialer{},
			Config: &tls.Config{
				ServerName: provider.SMTPHost,
				MinVersion: tls.VersionTLS12,
			},
		}
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	} else {
		conn, err = (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return failedNetworkHealth(ctx, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(deadline)

	client, err := smtp.NewClient(conn, provider.SMTPHost)
	if err != nil {
		return failedNetworkHealth(ctx, err)
	}
	defer client.Close()

	if provider.SMTPPort != 465 {
		if supportsTLS, _ := client.Extension("STARTTLS"); supportsTLS {
			if err := client.StartTLS(&tls.Config{ServerName: provider.SMTPHost, MinVersion: tls.VersionTLS12}); err != nil {
				return failedNetworkHealth(ctx, err)
			}
		}
	}

	auth := smtp.PlainAuth("", provider.SMTPUsername, provider.SMTPPassword, provider.SMTPHost)
	if err := client.Auth(auth); err != nil {
		if isTimeout(ctx, err) {
			return failedLiveHealth(reasonProbeTimeout)
		}
		return failedLiveHealth(reasonAuthenticationFailed)
	}
	_ = client.Quit()
	return providerLiveHealth{Status: liveCheckOK}
}

func probeTelegramHealth(ctx context.Context, provider ProviderConfig) providerLiveHealth {
	endpoint := strings.TrimRight(provider.TelegramAPIBase, "/") + "/bot" + provider.TelegramBotToken + "/getMe"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return failedLiveHealth(reasonUnexpectedResponse)
	}
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: healthCheckTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return failedNetworkHealth(ctx, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return failedLiveHealth(reasonAuthenticationFailed)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return failedLiveHealth(reasonUnexpectedResponse)
	}

	var result telegramResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&result); err != nil {
		return failedLiveHealth(reasonUnexpectedResponse)
	}
	if !result.OK {
		return failedLiveHealth(reasonAuthenticationFailed)
	}
	return providerLiveHealth{Status: liveCheckOK}
}

func failedNetworkHealth(ctx context.Context, err error) providerLiveHealth {
	if isTimeout(ctx, err) {
		return failedLiveHealth(reasonProbeTimeout)
	}
	return failedLiveHealth(reasonConnectivityFailed)
}

func isTimeout(ctx context.Context, err error) bool {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func failedLiveHealth(reason string) providerLiveHealth {
	return providerLiveHealth{Status: liveCheckFailed, ReasonCodes: []string{reason}}
}

func cloneProviderHealth(source map[string]providerLiveHealth) map[string]providerLiveHealth {
	cloned := make(map[string]providerLiveHealth, len(source))
	for name, result := range source {
		result.ReasonCodes = append([]string(nil), result.ReasonCodes...)
		cloned[name] = result
	}
	return cloned
}

func appendReason(reasons []string, reason string) []string {
	for _, existing := range reasons {
		if existing == reason {
			return reasons
		}
	}
	return append(reasons, reason)
}

func (a *App) currentTime() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}
