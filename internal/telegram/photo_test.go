package telegram

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExtractPhotosLocalAndRemote(t *testing.T) {
	root := t.TempDir()
	// sandbox-style rootfs layout: <root>/workspace
	png := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}
	if err := os.WriteFile(filepath.Join(root, "chart.png"), png, 0600); err != nil {
		t.Fatal(err)
	}
	answer := "Here is chart\n\n![my chart](chart.png)\n\nSee web ![pic](https://example.com/a.png) done"
	photos, cleaned, _ := ExtractPhotos(answer, root)
	if len(photos) != 2 {
		t.Fatalf("photos=%d cleaned=%q", len(photos), cleaned)
	}
	if photos[0].LocalPath == "" || photos[1].RemoteURL != "https://example.com/a.png" {
		t.Fatalf("%+v", photos)
	}
	if photos[0].Caption != "my chart" {
		t.Fatalf("caption=%q", photos[0].Caption)
	}
	if containsStr(cleaned, "![my chart]") || containsStr(cleaned, "![pic]") {
		t.Fatalf("markup not removed: %q", cleaned)
	}
	if !containsStr(cleaned, "Here is chart") || !containsStr(cleaned, "done") {
		t.Fatalf("text lost: %q", cleaned)
	}
}

func TestExtractPhotosRejectsTraversalAndBadScheme(t *testing.T) {
	root := t.TempDir()
	photos, cleaned, _ := ExtractPhotos("![x](../secret.png) and ![y](http://example.com/a.png)", root)
	if len(photos) != 0 {
		t.Fatalf("expected no photos, got %+v", photos)
	}
	if !containsStr(cleaned, "could not be posted") {
		t.Fatalf("expected fallback note, got %q", cleaned)
	}
}

func TestExtractPhotosMissingFileFallback(t *testing.T) {
	root := t.TempDir()
	photos, cleaned, _ := ExtractPhotos("![alt](missing.png)", root)
	if len(photos) != 0 {
		t.Fatalf("expected no photos")
	}
	if !containsStr(cleaned, "file not found") {
		t.Fatalf("got %q", cleaned)
	}
}

func containsStr(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
