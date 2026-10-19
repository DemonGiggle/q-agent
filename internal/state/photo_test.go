package state

import (
	"testing"

	"q-agent/internal/platform"
)

func TestQueueWithPhotosRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	route := platform.Message{Platform: "telegram", ChatID: 1, UpdateID: 9}
	photos := []PhotoAttachment{{RemoteURL: "https://example.com/a.png", Caption: "cap", Fallback: "fb"}}
	if err := s.QueueWithPhotos(route, []string{"hi"}, photos, nil); err != nil {
		t.Fatal(err)
	}
	r, ok := s.Pending()
	if !ok || len(r.Photos) != 1 || r.Total() != 2 {
		t.Fatalf("%+v %v", r, ok)
	}
	if err := s.Delivered(r.ID); err != nil {
		t.Fatal(err)
	}
	r, ok = s.Pending()
	if !ok || r.Next != 1 || r.Photos[0].RemoteURL != "https://example.com/a.png" {
		t.Fatalf("%+v %v", r, ok)
	}
	if err := s.Delivered(r.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Pending(); ok {
		t.Fatalf("expected empty")
	}
	// restart persistence
	s2, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s2.Pending(); ok {
		t.Fatalf("expected empty after restart")
	}
}

func TestQueueWithPhotosValidation(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, nil)
	route := platform.Message{Platform: "telegram", ChatID: 1, UpdateID: 10}
	if err := s.QueueWithPhotos(route, nil, nil, nil); err == nil {
		t.Fatalf("expected error")
	}
	many := make([]PhotoAttachment, 11)
	for i := range many {
		many[i] = PhotoAttachment{RemoteURL: "https://example.com/a.png"}
	}
	if err := s.QueueWithPhotos(route, []string{"hi"}, many, nil); err == nil {
		t.Fatalf("expected cap error")
	}
}
