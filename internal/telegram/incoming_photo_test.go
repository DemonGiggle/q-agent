package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDownloadPhoto(t *testing.T) {
	for _, tc := range []struct {
		name, path, data string
		size             int
		status           int
		wantErr          bool
	}{
		{"jpeg", "photos/a.jpg", "\xff\xd8\xff\xe0photo", 0, 200, false},
		{"nonimage", "photos/a.jpg", "not an image", 0, 200, true},
		{"oversized metadata", "photos/a.jpg", "", MaxIncomingPhotoBytes + 1, 200, true},
		{"oversized body", "photos/a.jpg", strings.Repeat("x", MaxIncomingPhotoBytes+1), 0, 200, true},
		{"traversal", "../secret", "", 0, 200, true},
		{"absolute URL", "https://example.com/a", "", 0, 200, true},
		{"server error", "photos/a.jpg", "", 0, 500, true},
		{"redirect", "photos/a.jpg", "", 0, 302, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			downloads := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/botsecret/getFile" {
					var request map[string]string
					_ = json.NewDecoder(r.Body).Decode(&request)
					if request["file_id"] != "id" {
						t.Errorf("bad ID: %v", request)
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"file_path": tc.path, "file_size": tc.size}})
					return
				}
				downloads++
				if r.URL.Path != "/file/botsecret/photos/a.jpg" {
					t.Errorf("unexpected path %s", r.URL.Path)
				}
				w.Header().Set("Location", "https://example.com/secret")
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.data)
			}))
			defer srv.Close()
			c := New("secret")
			c.BaseURL = srv.URL
			c.HTTP = srv.Client()
			image, err := c.DownloadPhoto(context.Background(), "id")
			if (err != nil) != tc.wantErr {
				t.Fatalf("image=%+v err=%v", image, err)
			}
			if err != nil && strings.Contains(err.Error(), "secret") {
				t.Fatal("token leaked")
			}
			if !tc.wantErr && (image.MediaType != "image/jpeg" || string(image.Data) != tc.data) {
				t.Fatal("image changed")
			}
			if (tc.path != "photos/a.jpg" || tc.size > MaxIncomingPhotoBytes) && downloads != 0 {
				t.Fatal("invalid file downloaded")
			}
		})
	}
}

func TestDownloadPhotoTransportErrorIsSanitized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close()
	c := New("do-not-leak")
	c.BaseURL = srv.URL
	_, err := c.DownloadPhoto(context.Background(), "a")
	if err == nil || strings.Contains(err.Error(), c.Token) {
		t.Fatalf("unsafe error: %v", err)
	}
}
