// Package zohocliq implements the inbound webhook handler and the outbound
// sender adapter for Zoho Cliq. The sender.Adapter interface is satisfied by
// Adapter; its init() in init.go self-registers with the sender registry so
// main.go only needs a blank import.
package zohocliq

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	miov1 "github.com/vanducng/mio/proto/gen/go/mio/v1"
)

const (
	cliqChannelType = "zoho_cliq"
	cliqMaxDeliver  = 5

	// Cliq REST base URL — override via CLIQ_API_BASE_URL in tests.
	defaultCliqBaseURL = "https://cliq.zoho.com"
)

// Adapter implements sender.Adapter for Zoho Cliq.
// Constructed in init.go; all fields are read-only after construction.
type Adapter struct {
	baseURL    string
	botToken   string // OAuth access token for bot API calls
	botName    string // bot unique_name (required for channelsbyname endpoint)
	httpClient *http.Client
	logger     *slog.Logger
}

// NewAdapter builds an Adapter from environment variables.
// Called from init.go — panics are acceptable (startup failure).
//
//   - CLIQ_BOT_TOKEN (required): Zoho Cliq bot OAuth token
//   - CLIQ_BOT_NAME  (required for outbound channel posts): bot unique name
//   - CLIQ_API_BASE_URL (optional): override for tests
func NewAdapter() *Adapter {
	botToken := os.Getenv("CLIQ_BOT_TOKEN")
	botName := os.Getenv("CLIQ_BOT_NAME")
	// Token + name are optional at init; outbound calls will fail explicitly
	// if either is absent so we get a Term-level error message in the log.
	baseURL := os.Getenv("CLIQ_API_BASE_URL")
	if baseURL == "" {
		baseURL = defaultCliqBaseURL
	}
	return &Adapter{
		baseURL:  baseURL,
		botToken: botToken,
		botName:  botName,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		logger: slog.Default(),
	}
}

// ChannelType returns the registry slug for this adapter.
func (a *Adapter) ChannelType() string { return cliqChannelType }

// MaxDeliver returns the max redelivery count for Cliq messages.
func (a *Adapter) MaxDeliver() int { return cliqMaxDeliver }

// RateLimitKey returns empty string — pool defaults to account_id.
// Cliq does not impose per-conversation limits in documented rate limits.
func (a *Adapter) RateLimitKey(_ *miov1.SendCommand) string { return "" }

// cliqSendRequest is the request body for POST /api/v2/chats/{chatid}/messages.
type cliqSendRequest struct {
	Text string `json:"text"`
}

// Send delivers a new outbound message to Cliq.
// Uses the bot endpoint POST /api/v2/channelsbyname/{name}/message?bot_unique_name={bot}
// (the /chats/{id}/messages endpoint is read-only / DM-only and rejects bot posts
// with request_method_invalid). Channel name must come from cmd.attributes
// "cliq_channel_name" (echo / MIU sets this from msg.conversation_display_name).
//
// Returns: best-effort message id. Cliq returns 204 No Content on success, so
// we synthesise from cmd.id — edit/replace flows that rely on the returned id
// will need an explicit lookup once Cliq exposes a write endpoint that returns
// the message id.
func (a *Adapter) Send(ctx context.Context, cmd *miov1.SendCommand) (string, error) {
	channelName := ""
	if attrs := cmd.GetAttributes(); attrs != nil {
		channelName = attrs["cliq_channel_name"]
	}
	if channelName == "" {
		return "", fmt.Errorf("cliq send: attributes.cliq_channel_name is required")
	}
	if a.botName == "" {
		return "", fmt.Errorf("cliq send: CLIQ_BOT_NAME env unset")
	}

	body := cliqSendRequest{Text: cmd.GetText()}
	reqBody, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("cliq send: marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/api/v2/channelsbyname/%s/message?bot_unique_name=%s",
		a.baseURL, channelName, a.botName)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return "", fmt.Errorf("cliq send: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if a.botToken != "" {
		// Cliq REST requires "Zoho-oauthtoken <token>", not standard Bearer.
		req.Header.Set("Authorization", "Zoho-oauthtoken "+a.botToken)
	}

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("cliq send: http: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	respBody, _ := io.ReadAll(resp.Body)

	if err := checkHTTPStatus(resp, respBody); err != nil {
		return "", err
	}

	// Bot endpoint returns 204 No Content on success — no message id available.
	// Fall back to cmd.id so callers have something to log; edit-flow callers
	// must look up the actual id separately (out of POC scope).
	syntheticID := cmd.GetId()
	a.logger.Info("cliq: sent outbound message",
		"cmd_id", cmd.GetId(),
		"channel_name", channelName,
		"http_status", resp.StatusCode,
		"synthetic_id", syntheticID,
	)
	return syntheticID, nil
}

// checkHTTPStatus converts non-2xx Cliq responses into typed errors.
// Captures the Retry-After header so the pool can honour it on 429.
func checkHTTPStatus(resp *http.Response, body []byte) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	retryAfter := 0
	if s := resp.Header.Get("Retry-After"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			retryAfter = n
		}
	}
	return &HTTPError{Code: resp.StatusCode, Body: string(body), RetryAfter: retryAfter}
}

// HTTPError carries the HTTP status code so the pool can route Nak vs Term.
// Implements sender.DeliveryError via the StatusCode/IsRetryable/IsRateLimited
// methods — no import cycle because sender package defines the interface only.
type HTTPError struct {
	Code        int    // HTTP status code
	Body        string // raw response body (for logging)
	RetryAfter  int    // Retry-After seconds (0 = header absent)
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("cliq: http %d: %s", e.Code, e.Body)
}

// StatusCode returns the HTTP status code (used by pool classify4xx via interface).
func (e *HTTPError) StatusCode() int { return e.Code }

// IsRetryable returns true for 5xx (transient) — pool Nak's these.
func (e *HTTPError) IsRetryable() bool { return e.Code >= 500 }

// IsRateLimited returns true when Cliq returned 429.
func (e *HTTPError) IsRateLimited() bool { return e.Code == http.StatusTooManyRequests }

// RetryAfterSeconds returns the Retry-After header value in seconds.
func (e *HTTPError) RetryAfterSeconds() int { return e.RetryAfter }
