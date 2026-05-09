package zohocliq

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	miov1 "github.com/vanducng/mio/proto/gen/go/mio/v1"
)

// cliqEditRequest is the request body for PATCH /api/v2/chats/{chatid}/messages/{msgid}.
// Cliq uses PATCH for in-place edits (verified against Cliq REST API docs;
// if a live call reveals PUT is required, swap the method string here — one-line change).
type cliqEditRequest struct {
	Text string `json:"text"`
}

// Edit updates an existing Cliq message in-place.
// cmd.EditOfExternalId must be the Cliq message id returned by a prior Send call.
// cmd.ConversationExternalId must be the Cliq chat id.
func (a *Adapter) Edit(ctx context.Context, cmd *miov1.SendCommand) error {
	convExtID := cmd.GetConversationExternalId()
	msgExtID := cmd.GetEditOfExternalId()

	if convExtID == "" {
		return fmt.Errorf("cliq edit: conversation_external_id is required")
	}
	if msgExtID == "" {
		return fmt.Errorf("cliq edit: edit_of_external_id is required (pool must resolve before calling Edit)")
	}

	body := cliqEditRequest{Text: cmd.GetText()}
	reqBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("cliq edit: marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/api/v2/chats/%s/messages/%s",
		a.baseURL, convExtID, msgExtID)

	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("cliq edit: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if a.botToken != "" {
		// Cliq REST requires "Zoho-oauthtoken <token>", not standard Bearer.
		req.Header.Set("Authorization", "Zoho-oauthtoken "+a.botToken)
	}

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("cliq edit: http: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	respBody, _ := io.ReadAll(resp.Body)

	if err := checkHTTPStatus(resp, respBody); err != nil {
		return err
	}

	a.logger.Info("cliq: edited outbound message",
		"cmd_id", cmd.GetId(),
		"conv_external_id", convExtID,
		"cliq_msg_id", msgExtID,
	)
	return nil
}
