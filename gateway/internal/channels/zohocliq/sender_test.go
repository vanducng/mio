package zohocliq

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	miov1 "github.com/vanducng/mio/proto/gen/go/mio/v1"
)

// newTestAdapter creates an Adapter pointed at a test HTTP server.
func newTestAdapter(baseURL string) *Adapter {
	return &Adapter{
		baseURL:  baseURL,
		botToken: "test-token",
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
		logger: slog.Default(),
	}
}

func TestSend_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.Header.Get("Authorization") != "Zoho-oauthtoken test-token" {
			t.Errorf("expected Zoho-oauthtoken, got %s", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "cliq-msg-999"})
	}))
	defer srv.Close()

	a := newTestAdapter(srv.URL)
	cmd := &miov1.SendCommand{
		Id:                   "cmd-1",
		ConversationExternalId: "chat-abc",
		Text:                 "hello",
	}

	extID, err := a.Send(context.Background(), cmd)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if extID != "cliq-msg-999" {
		t.Fatalf("expected cliq-msg-999, got %s", extID)
	}
}

func TestSend_MissingConversationExternalID(t *testing.T) {
	a := newTestAdapter("http://unused")
	cmd := &miov1.SendCommand{Id: "cmd-2", Text: "hi"}
	_, err := a.Send(context.Background(), cmd)
	if err == nil {
		t.Fatal("expected error for missing conversation_external_id")
	}
}

func TestSend_HTTP429_ReturnsHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"rate limited"}`))
	}))
	defer srv.Close()

	a := newTestAdapter(srv.URL)
	cmd := &miov1.SendCommand{
		Id:                   "cmd-429",
		ConversationExternalId: "chat-abc",
		Text:                 "hi",
	}

	_, err := a.Send(context.Background(), cmd)
	if err == nil {
		t.Fatal("expected error")
	}
	httpErr, ok := err.(*HTTPError)
	if !ok {
		t.Fatalf("expected *HTTPError, got %T", err)
	}
	if !httpErr.IsRateLimited() {
		t.Fatal("expected IsRateLimited() true")
	}
	if httpErr.RetryAfterSeconds() != 7 {
		t.Fatalf("expected RetryAfter=7, got %d", httpErr.RetryAfterSeconds())
	}
}

func TestSend_HTTP5xx_IsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	a := newTestAdapter(srv.URL)
	cmd := &miov1.SendCommand{
		Id:                   "cmd-5xx",
		ConversationExternalId: "chat-abc",
		Text:                 "hi",
	}
	_, err := a.Send(context.Background(), cmd)
	httpErr, ok := err.(*HTTPError)
	if !ok {
		t.Fatalf("expected *HTTPError, got %T", err)
	}
	if !httpErr.IsRetryable() {
		t.Fatal("expected IsRetryable() true for 500")
	}
	if httpErr.IsRateLimited() {
		t.Fatal("expected IsRateLimited() false for 500")
	}
}

func TestSend_HTTP401_NotRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	a := newTestAdapter(srv.URL)
	cmd := &miov1.SendCommand{
		Id:                   "cmd-401",
		ConversationExternalId: "chat-abc",
		Text:                 "hi",
	}
	_, err := a.Send(context.Background(), cmd)
	httpErr, ok := err.(*HTTPError)
	if !ok {
		t.Fatalf("expected *HTTPError")
	}
	if httpErr.IsRetryable() {
		t.Fatal("expected IsRetryable() false for 401")
	}
	if httpErr.StatusCode() != http.StatusUnauthorized {
		t.Fatalf("expected status 401, got %d", httpErr.StatusCode())
	}
}

func TestEdit_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("expected PATCH, got %s", r.Method)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	a := newTestAdapter(srv.URL)
	cmd := &miov1.SendCommand{
		Id:                   "cmd-edit",
		ConversationExternalId: "chat-abc",
		EditOfExternalId:     "cliq-msg-999",
		Text:                 "updated text",
	}
	if err := a.Edit(context.Background(), cmd); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEdit_MissingExternalID(t *testing.T) {
	a := newTestAdapter("http://unused")
	cmd := &miov1.SendCommand{
		Id:                   "cmd-edit",
		ConversationExternalId: "chat-abc",
		// EditOfExternalId intentionally empty
	}
	if err := a.Edit(context.Background(), cmd); err == nil {
		t.Fatal("expected error for missing edit_of_external_id")
	}
}

func TestAdapter_Interface(t *testing.T) {
	a := newTestAdapter("http://unused")
	if a.ChannelType() != cliqChannelType {
		t.Fatalf("expected %s, got %s", cliqChannelType, a.ChannelType())
	}
	if a.MaxDeliver() != cliqMaxDeliver {
		t.Fatalf("expected %d, got %d", cliqMaxDeliver, a.MaxDeliver())
	}
	cmd := &miov1.SendCommand{AccountId: "acct-1"}
	if key := a.RateLimitKey(cmd); key != "" {
		t.Fatalf("expected empty rate limit key, got %q", key)
	}
}
