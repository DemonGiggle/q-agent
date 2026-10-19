package telegram

import (
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"q-agent/internal/platform"
)

func TestSendPhotoRemoteURL(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/sendPhoto") {
			t.Errorf("path=%s", r.URL.Path)
		}
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			t.Errorf("ct=%s", r.Header.Get("Content-Type"))
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	defer server.Close()
	c := New("token")
	c.BaseURL = server.URL
	c.HTTP = server.Client()
	m := platform.Message{ChatID: -10, TopicID: 7}
	if err := c.SendPhoto(context.Background(), m, "", "https://example.com/a.png", "hi"); err != nil {
		t.Fatal(err)
	}
	if got["photo"] != "https://example.com/a.png" || got["caption"] != "hi" || got["parse_mode"] != "HTML" {
		t.Fatalf("%v", got)
	}
	if got["message_thread_id"] != float64(7) {
		t.Fatalf("%v", got)
	}
}

func TestSendPhotoMultipart(t *testing.T) {
	var chatID, caption, parseMode, thread string
	var filename string
	var fileBytes int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mr := multipart.NewReader(r.Body, multipartBoundary(r.Header.Get("Content-Type")))
		for {
			part, err := mr.NextPart()
			if err != nil {
				break
			}
			name := part.FormName()
			data, _ := readAll(part)
			switch name {
			case "chat_id":
				chatID = string(data)
			case "caption":
				caption = string(data)
			case "parse_mode":
				parseMode = string(data)
			case "message_thread_id":
				thread = string(data)
			case "photo":
				filename = part.FileName()
				fileBytes = len(data)
			}
		}
		w.Write([]byte(`{"ok":true,"result":{"message_id":2}}`))
	}))
	defer server.Close()
	dir := t.TempDir()
	fp := filepath.Join(dir, "p.png")
	os.WriteFile(fp, []byte{0x89, 0x50, 0x4e, 0x47, 1, 2, 3}, 0600)
	c := New("token")
	c.BaseURL = server.URL
	c.HTTP = server.Client()
	m := platform.Message{ChatID: -10, TopicID: 7}
	if err := c.SendPhoto(context.Background(), m, fp, "", "cap"); err != nil {
		t.Fatal(err)
	}
	if chatID != "-10" || caption != "cap" || parseMode != "HTML" || thread != "7" || filename != "p.png" || fileBytes == 0 {
		t.Fatalf("%q %q %q %q %q %d", chatID, caption, parseMode, thread, filename, fileBytes)
	}
}

func TestSendPhotoRejectsBadType(t *testing.T) {
	c := New("token")
	m := platform.Message{ChatID: 1}
	if err := c.SendPhoto(context.Background(), m, "/tmp/x.txt", "", ""); err == nil || !strings.Contains(err.Error(), "unsupported photo") {
		t.Fatalf("err=%v", err)
	}
	if err := c.SendPhoto(context.Background(), m, "", "http://example.com/a.png", ""); err == nil {
		t.Fatalf("expected invalid URL error")
	}
}

func multipartBoundary(ct string) string {
	for _, part := range strings.Split(ct, ";") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "boundary=") {
			return strings.TrimPrefix(part, "boundary=")
		}
	}
	return ""
}

func readAll(r interface{ Read([]byte) (int, error) }) ([]byte, error) {
	var out []byte
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			break
		}
	}
	return out, nil
}
