package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"q-agent/internal/platform"
	"strings"
	"time"
)

type Client struct {
	Token   string
	HTTP    *http.Client
	BaseURL string
}
type APIError struct {
	Code        int
	Description string
	RetryAfter  int
}

func (e *APIError) Error() string { return fmt.Sprintf("Telegram error %d: %s", e.Code, e.Description) }
func New(token string) *Client {
	return &Client{Token: token, HTTP: &http.Client{Timeout: 35 * time.Second}, BaseURL: "https://api.telegram.org"}
}
func (c *Client) call(ctx context.Context, method string, input, result any) error {
	data, err := json.Marshal(input)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/bot"+c.Token+"/"+method, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var envelope struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Code        int             `json:"error_code"`
		Description string          `json:"description"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&envelope); err != nil {
		return fmt.Errorf("decode Telegram response (%s): %w", resp.Status, err)
	}
	if !envelope.OK {
		return &APIError{Code: envelope.Code, Description: envelope.Description, RetryAfter: envelope.Parameters.RetryAfter}
	}
	if result != nil {
		return json.Unmarshal(envelope.Result, result)
	}
	return nil
}
func (c *Client) Me(ctx context.Context) (User, error) {
	var u User
	err := c.call(ctx, "getMe", map[string]any{}, &u)
	return u, err
}
func (c *Client) Updates(ctx context.Context, offset int64) ([]Update, error) {
	var updates []Update
	err := c.call(ctx, "getUpdates", map[string]any{"offset": offset, "timeout": 25, "allowed_updates": []string{"message", "callback_query"}}, &updates)
	return updates, err
}
func (c *Client) Send(ctx context.Context, m platform.Message, text string) error {
	input := map[string]any{"chat_id": m.ChatID, "text": text}
	if m.TopicID != 0 {
		input["message_thread_id"] = m.TopicID
	}
	return c.call(ctx, "sendMessage", input, nil)
}
func (c *Client) Typing(ctx context.Context, m platform.Message) error {
	input := map[string]any{"chat_id": m.ChatID, "action": "typing"}
	if m.TopicID != 0 {
		input["message_thread_id"] = m.TopicID
	}
	return c.call(ctx, "sendChatAction", input, nil)
}

// SendLive sends a best-effort live preview message and returns its Telegram
// message ID for later edits. Test stubs that return `true` yield ID 0,
// which disables later edits but still counts as sent.
func (c *Client) SendLive(ctx context.Context, m platform.Message, text string) (int64, error) {
	input := map[string]any{"chat_id": m.ChatID, "text": text}
	if m.TopicID != 0 {
		input["message_thread_id"] = m.TopicID
	}
	var raw json.RawMessage
	if err := c.call(ctx, "sendMessage", input, &raw); err != nil {
		return 0, err
	}
	if len(raw) == 0 || string(raw) == "true" {
		return 0, nil
	}
	var msg struct {
		MessageID int64 `json:"message_id"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return 0, nil
	}
	return msg.MessageID, nil
}

// EditLive edits a live preview message. Edits that leave text unchanged
// succeed silently. An ID of 0 (boolean test stub) is a no-op success.
func (c *Client) EditLive(ctx context.Context, m platform.Message, messageID int64, text string) error {
	if messageID == 0 {
		return nil
	}
	err := c.call(ctx, "editMessageText", map[string]any{"chat_id": m.ChatID, "message_id": messageID, "text": text}, nil)
	if api, ok := err.(*APIError); ok && api.Code == 400 && strings.Contains(api.Description, "message is not modified") {
		return nil
	}
	return err
}

// DeleteLive removes a live preview message. Best effort: callers ignore errors.
func (c *Client) DeleteLive(ctx context.Context, m platform.Message, messageID int64) error {
	if messageID == 0 {
		return nil
	}
	return c.call(ctx, "deleteMessage", map[string]any{"chat_id": m.ChatID, "message_id": messageID}, nil)
}

func (c *Client) EditModels(ctx context.Context, m platform.Message, text string, keyboard *platform.InlineKeyboard) error {
	err := c.call(ctx, "editMessageText", map[string]any{"chat_id": m.ChatID, "message_id": m.MessageID, "text": text, "reply_markup": keyboard}, nil)
	if api, ok := err.(*APIError); ok && api.Code == 400 && strings.Contains(api.Description, "message is not modified") {
		return nil
	}
	return err
}
func (c *Client) AnswerCallback(ctx context.Context, id, text string) error {
	return c.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": id, "text": text}, nil)
}
func (c *Client) SendKeyboard(ctx context.Context, m platform.Message, text string, keyboard *platform.InlineKeyboard) error {
	input := map[string]any{"chat_id": m.ChatID, "text": text, "reply_markup": keyboard}
	if m.TopicID != 0 {
		input["message_thread_id"] = m.TopicID
	}
	return c.call(ctx, "sendMessage", input, nil)
}
