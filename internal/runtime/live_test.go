package runtime

import (
	"context"
	"sync"
	"testing"
	"time"

	"q-agent/internal/llm"
	"q-agent/internal/platform"
	"q-agent/internal/state"
)

type fakeLiveSink struct {
	mu      sync.Mutex
	sends   []string
	edits   []string
	deletes int
	nextID  int64
}

func (f *fakeLiveSink) Typing(context.Context, platform.Message) error { return nil }
func (f *fakeLiveSink) SendLive(_ context.Context, _ platform.Message, text string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sends = append(f.sends, text)
	f.nextID++
	return f.nextID, nil
}
func (f *fakeLiveSink) EditLive(_ context.Context, _ platform.Message, id int64, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.edits = append(f.edits, text)
	return nil
}
func (f *fakeLiveSink) DeleteLive(_ context.Context, _ platform.Message, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes++
	return nil
}

func TestLiveReporterParagraphs(t *testing.T) {
	sink := &fakeLiveSink{}
	r := newLiveReporter(sink, platform.Message{ChatID: 1}, nil)
	r.SetStatus("Working… preparing workspace")
	// Force throttle window to pass for deterministic edits.
	r.mu.Lock()
	r.lastEdit = time.Now().Add(-2 * time.Second)
	r.mu.Unlock()
	r.ToolStart("shell", 1)
	r.mu.Lock()
	r.lastEdit = time.Now().Add(-2 * time.Second)
	r.mu.Unlock()
	r.StreamEvent(llm.StreamEvent{Kind: llm.StreamOutput, Text: "first paragraph\n\n"})
	r.mu.Lock()
	r.lastEdit = time.Now().Add(-2 * time.Second)
	r.mu.Unlock()
	r.StreamEvent(llm.StreamEvent{Kind: llm.StreamOutput, Text: "second paragraph"})
	// Thinking events are ignored.
	r.StreamEvent(llm.StreamEvent{Kind: llm.StreamThinking, Text: "secret"})
	r.Flush()
	if len(sink.sends) != 1 {
		t.Fatalf("sends=%d", len(sink.sends))
	}
	if len(sink.edits) == 0 {
		t.Fatal("no live edits")
	}
	last := sink.edits[len(sink.edits)-1]
	if len(last) == 0 || !contains(last, "second paragraph") {
		t.Fatalf("preview missing paragraphs: %q", last)
	}
	if contains(last, "secret") {
		t.Fatalf("thinking leaked: %q", last)
	}
	r.FinishSuccess()
	if sink.deletes != 1 {
		t.Fatalf("deletes=%d", sink.deletes)
	}
}

func TestLiveReporterErrorLeavesStatus(t *testing.T) {
	sink := &fakeLiveSink{}
	r := newLiveReporter(sink, platform.Message{ChatID: 1}, nil)
	r.SetStatus("Working…")
	r.mu.Lock()
	r.lastEdit = time.Now().Add(-2 * time.Second)
	r.mu.Unlock()
	r.FinishError("Task cancelled.")
	if len(sink.edits) != 1 || sink.edits[0] != "Task cancelled." {
		t.Fatalf("%+v", sink.edits)
	}
	if sink.deletes != 0 {
		t.Fatal("error should edit, not delete")
	}
}

func TestLiveDisabledWithoutSink(t *testing.T) {
	r := newLiveReporter(nil, platform.Message{}, nil)
	r.SetStatus("Working…")
	r.StreamEvent(llm.StreamEvent{Kind: llm.StreamOutput, Text: "hi"})
	r.Flush()
	r.FinishSuccess()
	r.FinishError("x")
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

type streamTestProvider struct {
	chunks []string
	answer string
}

func (p *streamTestProvider) Name() string { return "test" }
func (p *streamTestProvider) Complete(ctx context.Context, _ llm.Request, onText llm.StreamCallback) (llm.Response, error) {
	for _, c := range p.chunks {
		if onText != nil {
			onText(llm.StreamEvent{Kind: llm.StreamOutput, Text: c})
		}
	}
	return llm.Response{Message: llm.Message{Content: p.answer}}, nil
}

func TestRuntimeLiveStreamingKeepsDurableReply(t *testing.T) {
	r, _, _ := fixture(t)
	sink := &fakeLiveSink{}
	r.Sink = sink
	provider := &streamTestProvider{chunks: []string{"para one\n\n", "para two"}, answer: "para one\n\npara two"}
	r.Providers = func(_ state.Conversation) (llm.Provider, error) { return provider, nil }
	m := input(1, 10, 0, 1, "go")
	if err := r.Handle(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	r.Wait()
	reply, ok := r.Store.Pending()
	if !ok || len(reply.Parts) != 1 || reply.Parts[0] != "para one\n\npara two" {
		t.Fatalf("durable reply lost: %+v", reply)
	}
	if len(sink.sends) == 0 {
		t.Fatal("no live preview sent")
	}
	// Fast path (existing workspace) must skip "preparing workspace" and go
	// straight to contacting the model to avoid confusion after /models.
	for _, text := range append(append([]string{}, sink.sends...), sink.edits...) {
		if contains(text, "preparing workspace") {
			t.Fatalf("fast path showed preparing: %q", text)
		}
	}
	if !contains(sink.sends[0], "contacting") {
		t.Fatalf("expected contacting status first: %q", sink.sends[0])
	}
	if sink.deletes != 1 {
		t.Fatalf("preview not cleaned: %+v", sink)
	}
}

func TestRuntimeSlowProvisioningShowsPreparing(t *testing.T) {
	r, w, _ := fixture(t)
	sink := &fakeLiveSink{}
	r.Sink = sink
	w.block = make(chan struct{})
	w.started = make(chan struct{}, 1)
	provider := &streamTestProvider{answer: "done"}
	r.Providers = func(_ state.Conversation) (llm.Provider, error) { return provider, nil }
	m := input(1, 10, 0, 1, "go")
	if err := r.Handle(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	<-w.started
	// Simulate slow first-time provisioning (>0.8s timer).
	time.Sleep(1100 * time.Millisecond)
	close(w.block)
	r.Wait()
	found := false
	for _, text := range append(append([]string{}, sink.sends...), sink.edits...) {
		if contains(text, "preparing workspace") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("slow provisioning never announced: sends=%v edits=%v", sink.sends, sink.edits)
	}
}
