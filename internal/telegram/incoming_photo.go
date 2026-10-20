package telegram

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"time"

	"q-agent/internal/llm"
)

// Incoming images are bounded independently of Telegram's larger getFile limit.
const MaxIncomingPhotoBytes = 10 << 20

// DownloadPhoto resolves a Telegram file ID server-side. Neither the tokenized
// URL nor Telegram errors containing it may escape this adapter.
func (c *Client) DownloadPhoto(ctx context.Context, fileID string) (llm.Image, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var file struct {
		Path string `json:"file_path"`
		Size int64  `json:"file_size"`
	}
	if err := c.call(ctx, "getFile", map[string]any{"file_id": fileID}, &file); err != nil {
		return llm.Image{}, fmt.Errorf("Telegram image lookup failed")
	}
	if file.Size > MaxIncomingPhotoBytes {
		return llm.Image{}, fmt.Errorf("image exceeds 10 MB limit")
	}
	// Never accept a URL, traversal, query or fragment supplied as a file path.
	if file.Path == "" || strings.HasPrefix(file.Path, "/") || path.Clean(file.Path) != file.Path || strings.ContainsAny(file.Path, "\\:%?#") || file.Path == ".." || strings.HasPrefix(file.Path, "../") {
		return llm.Image{}, fmt.Errorf("invalid Telegram image path")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/file/bot"+c.Token+"/"+file.Path, nil)
	if err != nil {
		return llm.Image{}, fmt.Errorf("invalid Telegram image request")
	}
	// Do not forward credentials through redirects, even on custom API servers.
	client := *c.HTTP
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return llm.Image{}, fmt.Errorf("Telegram image download failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return llm.Image{}, fmt.Errorf("Telegram image download returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxIncomingPhotoBytes+1))
	if err != nil {
		return llm.Image{}, fmt.Errorf("Telegram image download failed")
	}
	if len(data) > MaxIncomingPhotoBytes {
		return llm.Image{}, fmt.Errorf("image exceeds 10 MB limit")
	}
	mediaType := http.DetectContentType(data)
	switch mediaType {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
	default:
		return llm.Image{}, fmt.Errorf("unsupported or invalid image content")
	}
	return llm.Image{MediaType: mediaType, Data: data}, nil
}
