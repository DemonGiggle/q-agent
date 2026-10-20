package runtime

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"q-agent/internal/llm"
	"q-agent/internal/platform"
	"q-agent/internal/state"
)

type steerResponse struct {
	text string
	err  error
}
type steerProvider struct {
	requests  chan llm.Request
	responses chan steerResponse
}

func (p *steerProvider) Name() string { return "ollama" }
func (p *steerProvider) Complete(ctx context.Context, req llm.Request, _ llm.StreamCallback) (llm.Response, error) {
	select {
	case p.requests <- req:
	case <-ctx.Done():
		return llm.Response{}, ctx.Err()
	}
	select {
	case response := <-p.responses:
		return llm.Response{Message: llm.Message{Content: response.text}}, response.err
	case <-ctx.Done():
		return llm.Response{}, ctx.Err()
	}
}
func setupSteer(t *testing.T) (*Runtime, *testWorkspace, *steerProvider) {
	t.Helper()
	r, w, _ := fixture(t)
	p := &steerProvider{requests: make(chan llm.Request, 16), responses: make(chan steerResponse, 16)}
	r.Providers = func(state.Conversation) (llm.Provider, error) { return p, nil }
	return r, w, p
}
func request(t *testing.T, p *steerProvider) llm.Request {
	t.Helper()
	select {
	case req := <-p.requests:
		return req
	case <-time.After(3 * time.Second):
		t.Fatal("missing provider request")
		return llm.Request{}
	}
}
func userTexts(req llm.Request) []string {
	var texts []string
	for _, m := range req.Messages {
		if m.Role == "user" {
			texts = append(texts, m.Content)
		}
	}
	return texts
}
func drainReplies(t *testing.T, store *state.Store) []string {
	t.Helper()
	var texts []string
	for {
		reply, ok := store.Pending()
		if !ok {
			return texts
		}
		texts = append(texts, reply.Parts[reply.Next])
		must(t, store.Delivered(reply.ID))
	}
}

func TestTelegramSteerSameRunIsolationHistoryAndDedup(t *testing.T) {
	r, w, p := setupSteer(t)
	r.Config.Telegram.AllowedUsers = []int64{1, 2}
	ctx := context.Background()
	must(t, r.Handle(ctx, input(1, -10, 7, 1, "original")))
	if got := userTexts(request(t, p)); strings.Join(got, "|") != "original" {
		t.Fatalf("initial: %v", got)
	}
	followup := input(2, -10, 7, 1, "correction")
	must(t, r.Handle(ctx, followup))
	must(t, r.Handle(ctx, followup)) // At most once, including while active.
	must(t, r.Handle(ctx, input(3, -10, 7, 1, "additional detail")))
	must(t, r.Handle(ctx, input(4, -99, 7, 99, "unauthorized")))
	must(t, r.Handle(ctx, input(5, -10, 7, 1, "/status")))
	must(t, r.Handle(ctx, input(6, -10, 7, 1, "/help")))
	must(t, r.Handle(ctx, input(7, -10, 8, 1, "other topic")))
	must(t, r.Handle(ctx, input(8, -10, 7, 2, "other owner")))
	p.responses <- steerResponse{text: "superseded"}
	if got := userTexts(request(t, p)); strings.Join(got, "|") != "original|correction\n\nadditional detail" {
		t.Fatalf("steering: %v", got)
	}
	p.responses <- steerResponse{text: "revised final"}
	r.Wait()
	if w.calls != 1 {
		t.Fatalf("steer started a separate run: %d", w.calls)
	}
	output := strings.Join(drainReplies(t, r.Store), "\n")
	if strings.Contains(output, "superseded") || !strings.Contains(output, "revised final") || !strings.Contains(output, "2 follow-up(s) pending") {
		t.Fatalf("output: %s", output)
	}
	reopened, err := state.Open(r.Config.StateDir, nil)
	must(t, err)
	c, err := reopened.Load(followup.ConversationKey())
	must(t, err)
	if len(c.Pairs) != 1 || c.Pairs[0].Answer != "revised final" || strings.Join(c.Pairs[0].Steers, "|") != "correction\n\nadditional detail" {
		t.Fatalf("saved: %+v", c)
	}
	mem, err := reopened.LoadMemory(followup.WorkspaceKey())
	must(t, err)
	if len(mem.Pending) != 1 || !strings.Contains(mem.Pending[0].Transcript, "user: original\nsteer: correction\n\nadditional detail\nassistant: revised final") {
		t.Fatalf("steering missing from durable source: %+v", mem)
	}
	r.Store = reopened // No goroutines remain; exercise reopened history.
	must(t, r.Handle(ctx, input(9, -10, 7, 1, "next task")))
	got := request(t, p)
	if strings.Join(userTexts(got), "|") != "original|correction\n\nadditional detail|next task" {
		t.Fatalf("history: %+v", got.Messages)
	}
	p.responses <- steerResponse{text: "next final"}
	r.Wait()
	select {
	case extra := <-p.requests:
		t.Fatalf("extra run: %+v", extra)
	default:
	}
}

func TestSteerDuringProvisioning(t *testing.T) {
	r, w, p := setupSteer(t)
	w.block, w.started = make(chan struct{}), make(chan struct{}, 1)
	must(t, r.Handle(context.Background(), input(1, -10, 7, 1, "original")))
	<-w.started
	must(t, r.Handle(context.Background(), input(2, -10, 7, 1, "provisioning follow-up")))
	close(w.block)
	if got := userTexts(request(t, p)); strings.Join(got, "|") != "original|provisioning follow-up" {
		t.Fatalf("input: %v", got)
	}
	p.responses <- steerResponse{text: "final"}
	r.Wait()
	if w.calls != 1 {
		t.Fatal("duplicate workspace provisioning")
	}
}

func TestSteerBoundsAndCancellation(t *testing.T) {
	r, _, p := setupSteer(t)
	ctx := context.Background()
	must(t, r.Handle(ctx, input(1, -10, 7, 1, "original")))
	request(t, p)
	for i := 0; i < maxPendingSteers; i++ {
		must(t, r.Handle(ctx, input(int64(i+2), -10, 7, 1, fmt.Sprintf("follow-up %d", i))))
	}
	must(t, r.Handle(ctx, input(100, -10, 7, 1, "one too many")))
	must(t, r.Handle(ctx, input(101, -10, 7, 1, "/cancel")))
	r.Wait()
	output := strings.Join(drainReplies(t, r.Store), "\n")
	if !strings.Contains(output, "Too many pending") || !strings.Contains(output, "Task cancelled.") || !strings.Contains(output, "16 pending follow-up") {
		t.Fatalf("output: %s", output)
	}
	c, err := r.Store.Load(input(0, -10, 7, 1, "").ConversationKey())
	must(t, err)
	if len(c.Pairs) != 0 {
		t.Fatal("cancelled steering saved successful history")
	}
	must(t, r.Handle(ctx, input(102, -10, 7, 1, "fresh task")))
	if got := userTexts(request(t, p)); strings.Join(got, "|") != "fresh task" {
		t.Fatalf("cancelled steer replayed: %v", got)
	}
	must(t, r.Handle(ctx, input(103, -10, 7, 1, strings.Repeat("x", maxPendingSteerBytes+1))))
	p.responses <- steerResponse{text: "fresh final"}
	r.Wait()
	if output := strings.Join(drainReplies(t, r.Store), "\n"); !strings.Contains(output, "Too many pending") {
		t.Fatalf("size bound: %s", output)
	}
}

func TestPendingSteerOnProviderFailureIsNotStartedSeparately(t *testing.T) {
	r, w, p := setupSteer(t)
	must(t, r.Handle(context.Background(), input(1, -10, 7, 1, "original")))
	request(t, p)
	must(t, r.Handle(context.Background(), input(2, -10, 7, 1, "follow-up")))
	p.responses <- steerResponse{err: fmt.Errorf("provider unavailable")}
	r.Wait()
	if w.calls != 1 {
		t.Fatal("failed task's steer started independently")
	}
	if output := strings.Join(drainReplies(t, r.Store), "\n"); !strings.Contains(output, "1 pending follow-up message(s) were not applied") {
		t.Fatalf("missing failure explanation: %s", output)
	}
}

// Hold final preview cleanup after the agent seals the run but before final
// outbox delivery and slot release, making the completion race deterministic.
type finishingSink struct {
	entered, release chan struct{}
	calls            atomic.Int32
}

func (s *finishingSink) Typing(context.Context, platform.Message) error { return nil }
func (s *finishingSink) SendLive(context.Context, platform.Message, string) (int64, error) {
	return 1, nil
}
func (s *finishingSink) EditLive(context.Context, platform.Message, int64, string) error { return nil }
func (s *finishingSink) DeleteLive(ctx context.Context, _ platform.Message, _ int64) error {
	if s.calls.Add(1) == 1 {
		close(s.entered)
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
func TestSteerRacingFinalAnswerStartsAfterCommitAndKeepsBothReplies(t *testing.T) {
	r, w, p := setupSteer(t)
	sink := &finishingSink{entered: make(chan struct{}), release: make(chan struct{})}
	r.Sink = sink
	must(t, r.Handle(context.Background(), input(1, -10, 7, 1, "original")))
	request(t, p)
	p.responses <- steerResponse{text: "first final"}
	select {
	case <-sink.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("finalization not reached")
	}
	const previous = "The riot began after an election."
	must(t, r.Handle(context.Background(), telegramReply(t, 2, -10, 7, "supergroup", "late follow-up", previous)))
	must(t, r.Handle(context.Background(), input(3, -10, 7, 1, "late detail")))
	select {
	case <-p.requests:
		t.Fatal("overlapping run before previous task finalized")
	default:
	}
	close(sink.release)
	req := request(t, p)
	texts := userTexts(req)
	if len(texts) != 2 || texts[0] != "original" || strings.Count(texts[1], previous) != 1 || !strings.HasSuffix(texts[1], "late follow-up\n\nlate detail") || req.Messages[2].Content != "first final" {
		t.Fatalf("committed history: %+v", req.Messages)
	}
	p.responses <- steerResponse{text: "second final"}
	r.Wait()
	if w.calls != 2 {
		t.Fatalf("runs: %d", w.calls)
	}
	output := strings.Join(drainReplies(t, r.Store), "\n")
	if !strings.Contains(output, "first final") || !strings.Contains(output, "second final") {
		t.Fatalf("ack suppressed final reply: %s", output)
	}
}

func TestScheduledTaskAcceptsSteerWithoutNewScheduledRun(t *testing.T) {
	r, w, p := setupSteer(t)
	enqueue(t, r, input(0, -10, 7, 1, ""), "agent", time.Now())
	must(t, r.dispatchScheduled(context.Background()))
	request(t, p)
	must(t, r.Handle(context.Background(), input(1, -10, 7, 1, "adjust scheduled work")))
	p.responses <- steerResponse{text: "superseded"}
	req := request(t, p)
	texts := userTexts(req)
	if len(texts) != 2 || texts[1] != "adjust scheduled work" || !strings.Contains(texts[0], "Scheduled task") {
		t.Fatalf("scheduled steering: %v", texts)
	}
	p.responses <- steerResponse{text: "scheduled final"}
	r.Wait()
	if w.calls != 1 || len(r.Store.PendingRuns()) != 0 {
		t.Fatal("steer became another scheduled run")
	}
	if output := strings.Join(drainReplies(t, r.Store), "\n"); !strings.Contains(output, "scheduled final") || strings.Contains(output, "superseded") {
		t.Fatalf("scheduled reply: %s", output)
	}
}

func TestPendingSteersMergePerBoundaryWithoutChangingSentBatch(t *testing.T) {
	r, _, p := setupSteer(t)
	ctx := context.Background()
	must(t, r.Handle(ctx, input(1, -10, 7, 1, "original")))
	request(t, p)
	must(t, r.Handle(ctx, input(2, -10, 7, 1, "first correction")))
	must(t, r.Handle(ctx, input(3, -10, 7, 1, "append detail")))
	p.responses <- steerResponse{text: "old answer"}
	sent := request(t, p)
	if got := userTexts(sent); strings.Join(got, "|") != "original|first correction\n\nappend detail" {
		t.Fatalf("first merged batch: %v", got)
	}
	must(t, r.Handle(ctx, input(4, -10, 7, 1, "second correction")))
	must(t, r.Handle(ctx, input(5, -10, 7, 1, "append more")))
	// The provider already owns an immutable copy of the earlier request.
	if got := userTexts(sent); strings.Join(got, "|") != "original|first correction\n\nappend detail" {
		t.Fatal("sent request was mutated")
	}
	p.responses <- steerResponse{text: "another old answer"}
	if got := userTexts(request(t, p)); strings.Join(got, "|") != "original|first correction\n\nappend detail|second correction\n\nappend more" {
		t.Fatalf("second merged batch: %v", got)
	}
	p.responses <- steerResponse{text: "final"}
	r.Wait()
	c, err := r.Store.Load(input(0, -10, 7, 1, "").ConversationKey())
	must(t, err)
	if len(c.Pairs) != 1 || len(c.Pairs[0].Steers) != 2 || c.Pairs[0].Steers[0] != "first correction\n\nappend detail" || c.Pairs[0].Steers[1] != "second correction\n\nappend more" {
		t.Fatalf("saved merged batches: %+v", c)
	}
}
