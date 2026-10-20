package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
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

// ParseModeHTML selects Telegram's HTML formatting style. HTML only
// needs &, <, > escaped (as &amp;, &lt;, &gt;), unlike MarkdownV2 which
// requires 18 reserved characters escaped; it supports bold, italic,
// underline, strikethrough, spoiler, links, inline code, pre blocks
// (with language), blockquotes and expandable blockquotes. There are
// no headings, tables, lists or images: see Format for the mapping.
const ParseModeHTML = "HTML"

func basePayload(m platform.Message, text string) map[string]any {
	input := map[string]any{
		"chat_id":              m.ChatID,
		"text":                 text,
		"parse_mode":           ParseModeHTML,
		"link_preview_options": map[string]any{"is_disabled": true},
	}
	if m.TopicID != 0 {
		input["message_thread_id"] = m.TopicID
	}
	return input
}

// isParseError reports Telegram "can't parse entities" failures, where
// the HTML markup was rejected. Callers fall back to plain text so a
// formatting bug never loses the message.
func isParseError(err error) bool {
	api, ok := err.(*APIError)
	if !ok || api == nil {
		return false
	}
	if api.Code != 400 {
		return false
	}
	desc := strings.ToLower(api.Description)
	return strings.Contains(desc, "can't parse") || strings.Contains(desc, "parse entities")
}

func (c *Client) sendHTML(ctx context.Context, method string, input map[string]any, result any) error {
	err := c.call(ctx, method, input, result)
	if err == nil || !isParseError(err) {
		return err
	}
	// Retry once as plain text with tags stripped so users never see
	// raw markup.
	plain := input
	if text, ok := input["text"].(string); ok {
		plain = map[string]any{}
		for k, v := range input {
			plain[k] = v
		}
		delete(plain, "parse_mode")
		delete(plain, "link_preview_options")
		plain["text"] = StripHTML(text)
	}
	return c.call(ctx, method, plain, result)
}

func (c *Client) Send(ctx context.Context, m platform.Message, text string) error {
	return c.sendHTML(ctx, "sendMessage", basePayload(m, text), nil)
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
	input := basePayload(m, text)
	var raw json.RawMessage
	if err := c.sendHTML(ctx, "sendMessage", input, &raw); err != nil {
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
	input := map[string]any{"chat_id": m.ChatID, "message_id": messageID, "text": text, "parse_mode": ParseModeHTML, "link_preview_options": map[string]any{"is_disabled": true}}
	err := c.sendHTML(ctx, "editMessageText", input, nil)
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
	err := c.sendHTML(ctx, "editMessageText", map[string]any{"chat_id": m.ChatID, "message_id": m.MessageID, "text": text, "parse_mode": ParseModeHTML, "link_preview_options": map[string]any{"is_disabled": true}, "reply_markup": keyboard}, nil)
	if api, ok := err.(*APIError); ok && api.Code == 400 && strings.Contains(api.Description, "message is not modified") {
		return nil
	}
	return err
}
func (c *Client) AnswerCallback(ctx context.Context, id, text string) error {
	return c.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": id, "text": text}, nil)
}
func (c *Client) SendKeyboard(ctx context.Context, m platform.Message, text string, keyboard *platform.InlineKeyboard) error {
	input := basePayload(m, text)
	input["reply_markup"] = keyboard
	return c.sendHTML(ctx, "sendMessage", input, nil)
}

// MaxPhotoBytes caps local photo uploads (Telegram sendPhoto limit is 10MB).
const MaxPhotoBytes = 10 << 20

// CaptionVisibleLimit is Telegram's 1024-character caption limit.
const CaptionVisibleLimit = 1024

var allowedPhotoExt = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".webp": true, ".gif": true,
}

func validPhotoURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return false
	}
	if u.User != nil {
		return false
	}
	if strings.Contains(raw, " ") || strings.Contains(raw, "\n") {
		return false
	}
	return true
}

func capCaptionHTML(caption string) string {
	if strings.TrimSpace(caption) == "" {
		return ""
	}
	if visibleLen(caption) <= CaptionVisibleLimit {
		return caption
	}
	plain := StripHTML(caption)
	runes := []rune(plain)
	if len(runes) > CaptionVisibleLimit {
		runes = append(runes[:CaptionVisibleLimit-1], '…')
	}
	return Escape(string(runes))
}

func photoPayload(m platform.Message, caption string) map[string]any {
	input := map[string]any{"chat_id": m.ChatID}
	if caption != "" {
		input["caption"] = caption
		input["parse_mode"] = ParseModeHTML
	}
	if m.TopicID != 0 {
		input["message_thread_id"] = m.TopicID
	}
	return input
}

// sendPhotoJSON sends a remote https:// URL via sendPhoto (no upload).
func (c *Client) sendPhotoJSON(ctx context.Context, m platform.Message, remoteURL, caption string) error {
	if !validPhotoURL(remoteURL) {
		return fmt.Errorf("invalid photo URL")
	}
	input := photoPayload(m, capCaptionHTML(caption))
	input["photo"] = remoteURL
	return c.sendCaptionHTML(ctx, "sendPhoto", input, nil)
}

// sendCaptionHTML retries caption parse failures as plain text.
func (c *Client) sendCaptionHTML(ctx context.Context, method string, input map[string]any, result any) error {
	err := c.call(ctx, method, input, result)
	if err == nil || !isParseError(err) {
		return err
	}
	plain := map[string]any{}
	for k, v := range input {
		plain[k] = v
	}
	delete(plain, "parse_mode")
	if capStr, ok := input["caption"].(string); ok {
		plain["caption"] = StripHTML(capStr)
	}
	return c.call(ctx, method, plain, nil)
}

// SendPhoto posts a native compressed Telegram photo. Exactly one of
// localPath or remoteURL must be set. Captions are HTML with fallback.
func (c *Client) SendPhoto(ctx context.Context, m platform.Message, localPath, remoteURL, caption string) error {
	caption = capCaptionHTML(caption)
	if localPath != "" && remoteURL != "" {
		return fmt.Errorf("photo must have either a local path or remote URL")
	}
	if remoteURL != "" {
		return c.sendPhotoJSON(ctx, m, remoteURL, caption)
	}
	if localPath == "" {
		return fmt.Errorf("photo must have a local path or remote URL")
	}
	ext := strings.ToLower(filepath.Ext(localPath))
	if !allowedPhotoExt[ext] {
		return fmt.Errorf("unsupported photo type %q (use jpg, png, webp, gif)", ext)
	}
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("photo is not a regular file")
	}
	if st.Size() > MaxPhotoBytes {
		return fmt.Errorf("photo exceeds 10MB limit")
	}
	if st.Size() == 0 {
		return fmt.Errorf("photo is empty")
	}
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	if err := w.WriteField("chat_id", fmt.Sprint(m.ChatID)); err != nil {
		return err
	}
	if m.TopicID != 0 {
		if err := w.WriteField("message_thread_id", fmt.Sprint(m.TopicID)); err != nil {
			return err
		}
	}
	if caption != "" {
		if err := w.WriteField("caption", caption); err != nil {
			return err
		}
		if err := w.WriteField("parse_mode", ParseModeHTML); err != nil {
			return err
		}
	}
	fw, err := w.CreateFormFile("photo", filepath.Base(localPath))
	if err != nil {
		return err
	}
	if _, err := io.CopyN(fw, f, MaxPhotoBytes+1); err != nil && err != io.EOF {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/bot"+c.Token+"/sendPhoto", &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
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
		api := &APIError{Code: envelope.Code, Description: envelope.Description, RetryAfter: envelope.Parameters.RetryAfter}
		if isParseError(api) && caption != "" {
			// One-shot plain-text retry without parse_mode.
			plain := StripHTML(caption)
			var body2 bytes.Buffer
			w2 := multipart.NewWriter(&body2)
			_ = w2.WriteField("chat_id", fmt.Sprint(m.ChatID))
			if m.TopicID != 0 {
				_ = w2.WriteField("message_thread_id", fmt.Sprint(m.TopicID))
			}
			_ = w2.WriteField("caption", plain)
			f2, openErr := os.Open(localPath)
			if openErr != nil {
				return openErr
			}
			defer f2.Close()
			fw2, cerr := w2.CreateFormFile("photo", filepath.Base(localPath))
			if cerr != nil {
				return cerr
			}
			if _, cerr := io.Copy(fw2, f2); cerr != nil {
				return cerr
			}
			if cerr := w2.Close(); cerr != nil {
				return cerr
			}
			req2, rerr := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/bot"+c.Token+"/sendPhoto", &body2)
			if rerr != nil {
				return rerr
			}
			req2.Header.Set("Content-Type", w2.FormDataContentType())
			resp2, derr := c.HTTP.Do(req2)
			if derr != nil {
				return derr
			}
			defer resp2.Body.Close()
			var env2 struct {
				OK          bool            `json:"ok"`
				Result      json.RawMessage `json:"result"`
				Code        int             `json:"error_code"`
				Description string          `json:"description"`
				Parameters  struct {
					RetryAfter int `json:"retry_after"`
				} `json:"parameters"`
			}
			if derr = json.NewDecoder(io.LimitReader(resp2.Body, 4<<20)).Decode(&env2); derr != nil {
				return fmt.Errorf("decode Telegram response (%s): %w", resp2.Status, derr)
			}
			if !env2.OK {
				return &APIError{Code: env2.Code, Description: env2.Description, RetryAfter: env2.Parameters.RetryAfter}
			}
			return nil
		}
		return api
	}
	return nil
}
