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
	botToken   string // Bearer token for bot API calls
	httpClient *http.Client
	logger     *slog.Logger
}

// NewAdapter builds an Adapter from environment variables.
// Called from init.go — panics are acceptable (startup failure).
//
//   - CLIQ_BOT_TOKEN (required): Zoho Cliq bot OAuth token
//   - CLIQ_API_BASE_URL (optional): override for tests
func NewAdapter() *Adapter {
	botToken := os.Getenv("CLIQ_BOT_TOKEN")
	// Token is optional at init time — gateway may boot before the secret
	// is mounted. Calls will fail with 401 if token absent; that's a Term.
	baseURL := os.Getenv("CLIQ_API_BASE_URL")
	if baseURL == "" {
		baseURL = defaultCliqBaseURL
	}
	return &Adapter{
		baseURL:  baseURL,
		botToken: botToken,
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

// cliqSendResponse is the minimal response shape we need.
type cliqSendResponse struct {
	ID string `json:"id"`
}

// Send delivers a new outbound message to Cliq.
// Returns the Cliq message id (external_id) for later edits.
// cmd.ConversationExternalId is the Cliq chat id.
func (a *Adapter) Send(ctx context.Context, cmd *miov1.SendCommand) (string, error) {
	if cmd.GetConversationExternalId() == "" {
		return "", fmt.Errorf("cliq send: conversation_external_id is required")
	}

	body := cliqSendRequest{Text: cmd.GetText()}
	reqBody, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("cliq send: marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/api/v2/chats/%s/messages",
		a.baseURL, cmd.GetConversationExternalId())

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return "", fmt.Errorf("cliq send: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if a.botToken != "" {
		req.Header.Set("Authorization", "Bearer "+a.botToken)
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

	var out cliqSendResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", fmt.Errorf("cliq send: decode response: %w", err)
	}
	if out.ID == "" {
		return "", fmt.Errorf("cliq send: empty message id in response")
	}

	a.logger.Info("cliq: sent outbound message",
		"cmd_id", cmd.GetId(),
		"conv_external_id", cmd.GetConversationExternalId(),
		"cliq_msg_id", out.ID,
	)
	return out.ID, nil
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
