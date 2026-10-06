// Package tg: minimal Telegram sender for vacancy cards (Bot API over raw
// HTTP, no SDK). Outbound only in v0.1 — the card carries a URL button, so
// no callback handling is needed.
package tg

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type Client struct {
	Token  string
	ChatID int64
	hc     *http.Client
}

func New(token string, chatID int64) *Client {
	return &Client{Token: token, ChatID: chatID, hc: &http.Client{Timeout: 30 * time.Second}}
}

// Card sends a vacancy card with one URL button. Returns the Telegram API
// response description on error.
func (c *Client) Card(ctx context.Context, text, buttonLabel, buttonURL string) error {
	if c.Token == "" || c.ChatID == 0 {
		return fmt.Errorf("telegram not configured")
	}
	payload := map[string]any{
		"chat_id":                  c.ChatID,
		"text":                     text,
		"disable_web_page_preview": true,
		"reply_markup": map[string]any{
			"inline_keyboard": [][]map[string]string{
				{{"text": buttonLabel, "url": buttonURL}},
			},
		},
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.telegram.org/bot"+c.Token+"/sendMessage", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var out struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return err
	}
	if !out.OK {
		return fmt.Errorf("telegram: %s", out.Description)
	}
	return nil
}
