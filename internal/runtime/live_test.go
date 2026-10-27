package runtime

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"q-agent/internal/llm"
	"q-agent/internal/platform"
	"q-agent/internal/state"
	"q-agent/internal/telegram"
)

type fakeLiveSink struct {
	mu      sync.Mutex
	sends   []string
	edits   []string
	deletes int
	nextID  int64
	updated chan struct{}
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
	if f.updated != nil {
		select {
		case f.updated <- struct{}{}:
		default:
		}
	}
	return nil
}

func (f *fakeLiveSink) snapshot() ([]string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.edits...), f.deletes
}

func waitForLiveEdit(t *testing.T, sink *fakeLiveSink, timeout time.Duration) []string {
	t.Helper()
	select {
	case <-sink.updated:
	case <-time.After(timeout):
		t.Fatal("timed out waiting for live edit")
	}
	edits, _ := sink.snapshot()
	return edits
}

func TestLiveReporterBatchesStreamAtParagraphBoundary(t *testing.T) {
	sink := &fakeLiveSink{updated: make(chan struct{}, 10)}
	r := newLiveReporter(sink, platform.Message{ChatID: 1}, nil)
	t.Cleanup(r.FinishSuccess)
	r.SetStatus("Working…")
	r.mu.Lock()
	r.lastEdit = time.Now().Add(-2 * liveThrottle)
	r.mu.Unlock()
	for _, chunk := range []string{"A", " longer", " paragraph", "\n"} {
		r.StreamEvent(llm.StreamEvent{Kind: llm.StreamOutput, Text: chunk})
	}
	if edits, _ := sink.snapshot(); len(edits) != 0 {
		t.Fatalf("partial paragraph edited immediately: %v", edits)
	}
	r.StreamEvent(llm.StreamEvent{Kind: llm.StreamOutput, Text: "\n"})
	edits := waitForLiveEdit(t, sink, time.Second)
	if len(edits) != 1 || !strings.Contains(edits[0], "A longer paragraph\n\n") {
		t.Fatalf("paragraph was not batched: %q", edits)
	}
}

func TestLiveReporterEventuallyShowsCoalescedToolStatus(t *testing.T) {
	sink := &fakeLiveSink{updated: make(chan struct{}, 10)}
	r := newLiveReporter(sink, platform.Message{ChatID: 1}, nil)
	t.Cleanup(r.FinishSuccess)
	r.SetStatus("Working…")
	r.ToolStart("read", 1)
	r.ToolEnd("read", 1, nil)
	r.ToolStart("shell", 2)
	if edits, _ := sink.snapshot(); len(edits) != 0 {
		t.Fatalf("rapid statuses were not coalesced: %v", edits)
	}
	edits := waitForLiveEdit(t, sink, 3*time.Second)
	if len(edits) != 1 || !strings.Contains(edits[0], "🔧 shell (2 tools)") {
		t.Fatalf("latest status not sent: %q", edits)
	}
}

func TestLiveReporterLongParagraphEventuallyAppears(t *testing.T) {
	sink := &fakeLiveSink{updated: make(chan struct{}, 10)}
	r := newLiveReporter(sink, platform.Message{ChatID: 1}, nil)
	t.Cleanup(r.FinishSuccess)
	r.SetStatus("Working…")
	r.StreamEvent(llm.StreamEvent{Kind: llm.StreamOutput, Text: "long paragraph"})
	edits := waitForLiveEdit(t, sink, liveStreamDelay+time.Second)
	if len(edits) != 1 || !strings.Contains(edits[0], "long paragraph") {
		t.Fatalf("partial paragraph did not appear: %q", edits)
	}
}

type rateLimitedLiveSink struct {
	*fakeLiveSink
	attempts chan struct{}
	failed   bool
}

func (f *rateLimitedLiveSink) EditLive(ctx context.Context, m platform.Message, id int64, text string) error {
	if !f.failed {
		f.failed = true
		f.attempts <- struct{}{}
		return &telegram.APIError{Code: 429, RetryAfter: 1}
	}
	return f.fakeLiveSink.EditLive(ctx, m, id, text)
}

func TestLiveReporterRetriesRateLimitedEdit(t *testing.T) {
	base := &fakeLiveSink{updated: make(chan struct{}, 1)}
	sink := &rateLimitedLiveSink{fakeLiveSink: base, attempts: make(chan struct{}, 1)}
	r := newLiveReporter(sink, platform.Message{ChatID: 1}, nil)
	t.Cleanup(r.FinishSuccess)
	r.SetStatus("Working…")
	r.ToolStart("shell", 1)
	select {
	case <-sink.attempts:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for first edit")
	}
	edits := waitForLiveEdit(t, base, 2*time.Second)
	if len(edits) != 1 || !strings.Contains(edits[0], "🔧 shell") {
		t.Fatalf("rate-limited status was not retried: %q", edits)
	}
}

func TestLiveReporterFinishCancelsPendingUpdates(t *testing.T) {
	sink := &fakeLiveSink{updated: make(chan struct{}, 10)}
	r := newLiveReporter(sink, platform.Message{ChatID: 1}, nil)
	r.SetStatus("Working…")
	r.ToolStart("shell", 1)
	r.FinishSuccess()
	// A late provisioning timer or stream callback must not recreate a preview.
	r.SetStatus("late")
	r.StreamEvent(llm.StreamEvent{Kind: llm.StreamOutput, Text: "late\n\n"})
	select {
	case <-sink.updated:
		t.Fatal("edited a finished preview")
	case <-time.After(liveThrottle + 200*time.Millisecond):
	}
	edits, deletes := sink.snapshot()
	if len(edits) != 0 || deletes != 1 || len(sink.sends) != 1 {
		t.Fatalf("unexpected finished preview: sends=%v edits=%v deletes=%d", sink.sends, edits, deletes)
	}
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
	m := input(1, -10, 0, 1, "go")
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
	m := input(1, -10, 0, 1, "go")
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
