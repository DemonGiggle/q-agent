package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNormalizeGenericFiles(t *testing.T) {
	bot := User{ID: 99, Username: "our_bot"}
	raw := `{"chat":{"id":1,"type":"private"},"text":"handle this","document":{"file_id":"d1","file_name":"logo.png","mime_type":"image/png","file_size":123}}`
	var m Message
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	m.From = &User{ID: 1}
	out, ok := Normalize(Update{ID: 5, Message: &m}, bot)
	if !ok || len(out.Files) != 1 || out.Files[0].FileID != "d1" || out.Files[0].Name != "logo.png" {
		t.Fatalf("document lost: %+v %v", out, ok)
	}
	// Group without mention still rejected even with a file.
	gm := Message{From: &User{ID: 1}, Chat: Chat{ID: -10, Type: "group"}, Document: &FileMeta{FileID: "d1", FileName: "a.txt"}}
	if _, ok := Normalize(Update{Message: &gm}, bot); ok {
		t.Fatal("unaddressed file accepted")
	}
	// Reply to a document with a mention picks up the reply file.
	rm := Message{From: &User{ID: 1}, Chat: Chat{ID: -10, Type: "group"}, Text: "@our_bot use this", Reply: &Message{From: &User{ID: 2}, Document: &FileMeta{FileID: "d2"}}}
	if out, ok := Normalize(Update{Message: &rm}, bot); !ok || len(out.Files) != 1 || out.Files[0].FileID != "d2" {
		t.Fatalf("reply file lost: %+v %v", out, ok)
	}
	// Video/voice fallbacks keep a usable name.
	vm := Message{From: &User{ID: 1}, Chat: Chat{ID: 1, Type: "private"}, Voice: &FileMeta{FileID: "v1"}}
	out, ok = Normalize(Update{Message: &vm}, bot)
	if !ok || len(out.Files) != 1 || out.Files[0].Name == "" {
		t.Fatalf("voice fallback: %+v %v", out, ok)
	}
}

func TestExtractTapFileMarker(t *testing.T) {
	m := &Message{From: &User{ID: 1}, Chat: Chat{ID: 1, Type: "private"}, Document: &FileMeta{FileID: "d1", FileName: "a.pdf"}}
	_, text, ok := ExtractTap(Update{ID: 1, Message: m})
	if !ok || !strings.Contains(text, "[file]") {
		t.Fatalf("tap=%q ok=%v", text, ok)
	}
}

func TestDownloadFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/botsecret/getFile" {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"file_path": "docs/a.bin", "file_size": 4}})
			return
		}
		w.Write([]byte{1, 2, 3, 4})
	}))
	defer srv.Close()
	c := New("secret")
	c.BaseURL = srv.URL
	c.HTTP = srv.Client()
	data, err := c.DownloadFile(context.Background(), "id")
	if err != nil || len(data) != 4 {
		t.Fatalf("download=%v err=%v", len(data), err)
	}
}

func TestDownloadFileRejects(t *testing.T) {
	for _, tc := range []struct{ name, path string }{
		{"traversal", "../secret"},
		{"absolute URL", "https://example.com/a"},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "getFile") {
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"file_path": tc.path}})
				return
			}
			t.Error("invalid file downloaded")
		}))
		c := New("secret")
		c.BaseURL = srv.URL
		c.HTTP = srv.Client()
		if _, err := c.DownloadFile(context.Background(), "id"); err == nil {
			t.Fatalf("%s accepted", tc.name)
		} else if strings.Contains(err.Error(), "secret") {
			t.Fatal("token leaked")
		}
		srv.Close()
	}
}

func TestSanitizeFileName(t *testing.T) {
	for in, want := range map[string]string{
		"logo.png":                        "logo.png",
		"../evil.sh":                      "evil.sh",
		"a/b\\c.txt":                      "c.txt",
		"":                                "file.bin",
		"..":                              "file.bin",
		"  spaced name .pdf ":             "spaced name .pdf",
		"emoji-😀.txt":                     "emoji-_.txt",
		strings.Repeat("a", 200) + ".txt": strings.Repeat("a", 96) + ".txt",
	} {
		if got := SanitizeFileName(in); got != want {
			t.Errorf("sanitize %q = %q, want %q", in, got, want)
		}
	}
}
