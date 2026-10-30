package telegram

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"time"
)

// Generic file attachments are bounded independently of Telegram's larger
// getFile limit. Photos (10 MB) and any other file both land in the chat
// workspace inbox for the agent to read with tools; photos double as vision
// input.
const MaxIncomingFileBytes = 20 << 20

// MaxFilesPerMessage caps generic attachments per Telegram message so one
// update cannot exhaust disk or context. Photos keep their own limit of 2.
const MaxFilesPerMessage = 5

// MaxInboxBytes caps the per-chat inbox directory. The runtime enforces it
// before saving so one chat cannot fill the host disk.
const MaxInboxBytes = 100 << 20

// DownloadFile resolves a Telegram file ID server-side and returns raw bytes.
// Neither the tokenized URL nor Telegram errors containing it may escape.
func (c *Client) DownloadFile(ctx context.Context, fileID string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var file struct {
		Path string `json:"file_path"`
		Size int64  `json:"file_size"`
	}
	if err := c.call(ctx, "getFile", map[string]any{"file_id": fileID}, &file); err != nil {
		return nil, fmt.Errorf("Telegram file lookup failed")
	}
	if file.Size > MaxIncomingFileBytes {
		return nil, fmt.Errorf("file exceeds 20 MB limit")
	}
	// Never accept a URL, traversal, query or fragment supplied as a file path.
	if file.Path == "" || strings.HasPrefix(file.Path, "/") || path.Clean(file.Path) != file.Path || strings.ContainsAny(file.Path, "\\:%?#") || file.Path == ".." || strings.HasPrefix(file.Path, "../") {
		return nil, fmt.Errorf("invalid Telegram file path")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/file/bot"+c.Token+"/"+file.Path, nil)
	if err != nil {
		return nil, fmt.Errorf("invalid Telegram file request")
	}
	// Do not forward credentials through redirects, even on custom API servers.
	client := *c.HTTP
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Telegram file download failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Telegram file download returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxIncomingFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("Telegram file download failed")
	}
	if len(data) > MaxIncomingFileBytes {
		return nil, fmt.Errorf("file exceeds 20 MB limit")
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("empty Telegram file")
	}
	return data, nil
}

// SanitizeFileName strips directories and unsafe runes so Telegram-supplied
// names cannot escape the chat inbox. It preserves a short extension when
// present and always returns a non-empty base name.
func SanitizeFileName(name string) string {
	name = strings.TrimSpace(name)
	// Drop any directory components the client may have sent.
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	if i := strings.LastIndex(name, "\\"); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return "file.bin"
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.' || r == '-' || r == '_' || r == '+' || r == '(' || r == ')' || r == ' ':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := strings.TrimSpace(b.String())
	out = strings.Trim(out, ".")
	if out == "" {
		return "file.bin"
	}
	// Cap length while keeping an extension when possible.
	if len(out) > 100 {
		ext := ""
		if i := strings.LastIndex(out, "."); i >= 0 && len(out)-i <= 12 {
			ext = out[i:]
			out = out[:100-len(ext)] + ext
		} else {
			out = out[:100]
		}
		out = strings.Trim(out, ".")
		if out == "" {
			return "file.bin"
		}
	}
	return out
}
