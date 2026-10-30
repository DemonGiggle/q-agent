package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"q-agent/internal/llm"
	"q-agent/internal/platform"
	"q-agent/internal/telegram"
)

type photoSink struct {
	calls int
	fail  bool
}

func (*photoSink) Typing(context.Context, platform.Message) error { return nil }
func (s *photoSink) DownloadPhoto(context.Context, string) (llm.Image, error) {
	s.calls++
	if s.fail {
		return llm.Image{}, fmt.Errorf("download failed")
	}
	return llm.Image{MediaType: "image/jpeg", Data: []byte("test-image")}, nil
}

// photoWorkspace replaces the mock workspace with a real temp rootfs so
// savePhotos can persist to inbox/ without polluting the package dir.
func photoWorkspace(t *testing.T, r *Runtime) string {
	t.Helper()
	dir := t.TempDir()
	rootfs := filepath.Join(dir, "rootfs")
	if err := os.MkdirAll(filepath.Join(rootfs, "workspace"), 0700); err != nil {
		t.Fatal(err)
	}
	r.Workspaces = &dirWorkspace{dir: rootfs}
	return rootfs
}

func TestIncomingPhotoReachesModel(t *testing.T) {
	r, _, p := fixture(t)
	rootfs := photoWorkspace(t, r)
	sink := &photoSink{}
	r.Sink = sink
	m, ok := telegram.Normalize(telegram.Update{ID: 1, Message: &telegram.Message{
		ID: 1, Chat: telegram.Chat{ID: -10, Type: "supergroup"}, From: &telegram.User{ID: 1},
		Caption: "@bot describe", CaptionEntities: []telegram.Entity{{Type: "mention", Offset: 0, Length: 4}},
		Photo: []telegram.PhotoSize{{FileID: "file", Width: 100, Height: 100}},
	}}, telegram.User{ID: 99, Username: "bot"})
	if !ok {
		t.Fatal("photo dropped by normalization")
	}
	if err := r.Handle(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	r.Wait()
	if sink.calls != 1 || len(p.requests) != 1 {
		t.Fatalf("downloads=%d requests=%d", sink.calls, len(p.requests))
	}
	messages := p.requests[0].Messages
	last := messages[len(messages)-1]
	if len(last.Images) != 1 || string(last.Images[0].Data) != "test-image" {
		t.Fatalf("image lost: %+v", last)
	}
	// Photo must also persist to inbox/ so tools can use it later.
	inbox := filepath.Join(rootfs, "workspace", "inbox")
	entries, err := os.ReadDir(inbox)
	if err != nil || len(entries) != 1 {
		t.Fatalf("inbox entries=%v err=%v", entries, err)
	}
	name := entries[0].Name()
	if !strings.HasPrefix(name, "photo-1-1.") {
		t.Fatalf("unexpected photo name %q", name)
	}
	data, err := os.ReadFile(filepath.Join(inbox, name))
	if err != nil || string(data) != "test-image" {
		t.Fatalf("inbox bytes=%q err=%v", string(data), err)
	}
	if !strings.Contains(last.Content, "inbox/"+name) {
		t.Fatalf("prompt missing inbox path: %q", last.Content)
	}
}

func TestIncomingPhotoAuthorizationAndFailure(t *testing.T) {
	r, _, p := fixture(t)
	photoWorkspace(t, r)
	sink := &photoSink{fail: true}
	r.Sink = sink
	m := input(1, -99, 0, 1, "describe")
	m.PhotoIDs = []string{"file"}
	if err := r.Handle(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if sink.calls != 0 {
		t.Fatal("unauthorized download")
	}
	m.UpdateID = 2
	m.ChatID = 1
	if err := r.Handle(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	r.Wait()
	if sink.calls != 1 || len(p.requests) != 0 {
		t.Fatal("failed image sent to model")
	}
	_, ok := r.Store.Pending()
	if !ok {
		t.Fatal("download error silently dropped")
	}
}

func TestSavePhotosToInbox(t *testing.T) {
	r, _, _ := fixture(t)
	rootfs := photoWorkspace(t, r)
	sink := &photoSink{}
	r.Sink = sink
	m := platform.Message{Platform: "telegram", ChatID: 1, UserID: 1, UpdateID: 7, MessageID: 42, Text: "use this", PhotoIDs: []string{"p1"}}
	if err := r.savePhotos(context.Background(), &m); err != nil {
		t.Fatal(err)
	}
	if len(m.PhotoIDs) != 0 {
		t.Fatal("PhotoIDs not cleared")
	}
	if len(m.Images) != 1 {
		t.Fatalf("vision missing: %+v", m.Images)
	}
	if !strings.Contains(m.Text, "inbox/photo-42-1.jpg") {
		t.Fatalf("path not folded: %q", m.Text)
	}
	data, err := os.ReadFile(filepath.Join(rootfs, "workspace", "inbox", "photo-42-1.jpg"))
	if err != nil || string(data) != "test-image" {
		t.Fatalf("saved=%q err=%v", string(data), err)
	}
}

func TestSavePhotosTooMany(t *testing.T) {
	r, _, _ := fixture(t)
	photoWorkspace(t, r)
	r.Sink = &photoSink{}
	m := platform.Message{Platform: "telegram", ChatID: 1, UserID: 1, Text: "many", PhotoIDs: []string{"a", "b", "c"}}
	if err := r.savePhotos(context.Background(), &m); err == nil || !strings.Contains(err.Error(), "maximum 2") {
		t.Fatalf("expected limit error, got %v", err)
	}
}

func TestPhotoExtension(t *testing.T) {
	for mt, want := range map[string]string{
		"image/jpeg": "jpg",
		"image/png":  "png",
		"image/gif":  "gif",
		"image/webp": "webp",
		"other":      "jpg",
	} {
		if got := photoExtension(mt); got != want {
			t.Errorf("extension %q = %q, want %q", mt, got, want)
		}
	}
}

func TestPhotoCommandSkipsDownload(t *testing.T) {
	r, _, p := fixture(t)
	photoWorkspace(t, r)
	sink := &photoSink{}
	r.Sink = sink
	m := input(1, 1, 0, 1, "/status")
	m.PhotoIDs = []string{"file"}
	if err := r.Handle(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	r.Wait()
	if sink.calls != 0 {
		t.Fatalf("command triggered download")
	}
	if len(p.requests) != 0 {
		t.Fatal("command started a task")
	}
}

func TestTakeSteerPreservesImages(t *testing.T) {
	r, _, _ := fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := input(1, 1, 0, 1, "photo")
	m.Images = []llm.Image{{MediaType: "image/png", Data: []byte("image")}}
	r.active = map[string]task{m.WorkspaceKey(): {ctx: ctx, pending: []platform.Message{m, input(2, 1, 0, 1, "more")}}}
	inputs := r.takeSteer(m.WorkspaceKey(), false)
	if len(inputs) != 1 || inputs[0].Content != "photo\n\nmore" || len(inputs[0].Images) != 1 {
		t.Fatalf("lost input: %+v", inputs)
	}
	if len(r.takeSteer(m.WorkspaceKey(), true)) != 0 || !r.active[m.WorkspaceKey()].finishing {
		t.Fatal("sealing failed")
	}
	delete(r.active, m.WorkspaceKey())
}

type finishingPhotoSink struct{ *finishingSink }

func (*finishingPhotoSink) DownloadPhoto(context.Context, string) (llm.Image, error) {
	return llm.Image{MediaType: "image/png", Data: []byte("late-image")}, nil
}

func TestLatePhotoStartsAfterFinalCommit(t *testing.T) {
	r, _, p := setupSteer(t)
	rootfs := photoWorkspace(t, r)
	sink := &finishingPhotoSink{&finishingSink{entered: make(chan struct{}), release: make(chan struct{})}}
	r.Sink = sink
	must(t, r.Handle(context.Background(), input(1, -10, 7, 1, "original")))
	request(t, p)
	p.responses <- steerResponse{text: "first final"}
	select {
	case <-sink.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("finalization not reached")
	}
	m := input(2, -10, 7, 1, "late photo")
	m.PhotoIDs = []string{"photo"}
	must(t, r.Handle(context.Background(), m))
	select {
	case <-p.requests:
		t.Fatal("overlapping run")
	default:
	}
	close(sink.release)
	req := request(t, p)
	last := req.Messages[len(req.Messages)-1]
	if len(last.Images) != 1 || string(last.Images[0].Data) != "late-image" {
		t.Fatalf("late image lost: %+v", last)
	}
	if !strings.Contains(last.Content, "inbox/photo-") {
		t.Fatalf("late prompt missing inbox path: %q", last.Content)
	}
	// Late photo must also persist for tools.
	entries, err := os.ReadDir(filepath.Join(rootfs, "workspace", "inbox"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("late inbox=%v err=%v", entries, err)
	}
	p.responses <- steerResponse{text: "second final"}
	r.Wait()
}
