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
	"net/url"
	"strings"
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

// Card sends a vacancy card with one URL button.
func (c *Client) Card(ctx context.Context, text, buttonLabel, buttonURL string) error {
	return c.Keyboard(ctx, text, [][]map[string]string{
		{{"text": buttonLabel, "url": buttonURL}},
	})
}

// CardWithApply sends a card with an apply callback button plus a URL row.
func (c *Client) CardWithApply(ctx context.Context, text, applyLabel, callbackData, urlLabel, url string) error {
	return c.Keyboard(ctx, text, [][]map[string]string{
		{{"text": applyLabel, "callback_data": callbackData}},
		{{"text": urlLabel, "url": url}},
	})
}

// AnswerCallback closes a callback query's spinner.
func (c *Client) AnswerCallback(ctx context.Context, callbackID, text string) {
	body, _ := json.Marshal(map[string]string{"callback_query_id": callbackID, "text": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.telegram.org/bot"+c.Token+"/answerCallbackQuery", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err == nil {
		resp.Body.Close()
	}
}

// Keyboard sends text with an arbitrary inline keyboard.
func (c *Client) Keyboard(ctx context.Context, text string, rows [][]map[string]string) error {
	if c.Token == "" || c.ChatID == 0 {
		return fmt.Errorf("telegram not configured")
	}
	payload := map[string]any{
		"chat_id":                  c.ChatID,
		"text":                     text,
		"disable_web_page_preview": true,
		"reply_markup": map[string]any{
			"inline_keyboard": rows,
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

// Poll runs getUpdates long-polling, invoking fn for every callback query
// whose data starts with prefix. Blocks until ctx is done.
func (c *Client) Poll(ctx context.Context, prefix string, fn func(callbackID, data string)) {
	offset := 0
	for ctx.Err() == nil {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?timeout=50&offset=%d&allowed_updates=%s",
				c.Token, offset, url.QueryEscape(`["callback_query"]`)), nil)
		if err != nil {
			return
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			time.Sleep(5 * time.Second)
			continue
		}
		var out struct {
			OK     bool `json:"ok"`
			Result []struct {
				UpdateID      int `json:"update_id"`
				CallbackQuery *struct {
					ID   string `json:"id"`
					Data string `json:"data"`
				} `json:"callback_query"`
			} `json:"result"`
		}
		err = json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		if err != nil || !out.OK {
			time.Sleep(5 * time.Second)
			continue
		}
		for _, u := range out.Result {
			offset = u.UpdateID + 1
			if u.CallbackQuery != nil && strings.HasPrefix(u.CallbackQuery.Data, prefix) {
				fn(u.CallbackQuery.ID, u.CallbackQuery.Data)
			}
		}
	}
}

// Plain sends text without any keyboard.
func (c *Client) Plain(ctx context.Context, text string) error {
	return c.Keyboard(ctx, text, nil)
}
