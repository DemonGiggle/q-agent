package runtime

import (
	"context"
	"fmt"
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

func TestIncomingPhotoReachesModel(t *testing.T) {
	r, _, p := fixture(t)
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
}

func TestIncomingPhotoAuthorizationAndFailure(t *testing.T) {
	r, _, p := fixture(t)
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
	p.responses <- steerResponse{text: "second final"}
	r.Wait()
}
