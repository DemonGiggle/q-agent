package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	"q-agent/internal/llm"
	"q-agent/internal/state"
)

type selectiveQueueProvider struct {
	requests chan llm.Request
	blockA   chan struct{}
	blockB   chan struct{}
}

func (p *selectiveQueueProvider) Name() string { return "ollama" }

func (p *selectiveQueueProvider) Complete(ctx context.Context, req llm.Request, _ llm.StreamCallback) (llm.Response, error) {
	select {
	case p.requests <- req:
	case <-ctx.Done():
		return llm.Response{}, ctx.Err()
	}
	joined := ""
	for _, m := range req.Messages {
		if m.Role == "user" {
			joined += m.Content + "\n"
		}
	}
	if strings.Contains(joined, "task A") {
		select {
		case <-p.blockA:
		case <-ctx.Done():
			return llm.Response{}, ctx.Err()
		}
		return llm.Response{Message: llm.Message{Content: "A done"}}, nil
	}
	if strings.Contains(joined, "task B") {
		select {
		case <-p.blockB:
		case <-ctx.Done():
			return llm.Response{}, ctx.Err()
		}
		return llm.Response{Message: llm.Message{Content: "B done"}}, nil
	}
	if strings.Contains(joined, "queued free workspace") {
		return llm.Response{Message: llm.Message{Content: "C done"}}, nil
	}
	return llm.Response{Message: llm.Message{Content: "queued same done"}}, nil
}

func TestQueuedDifferentTopicRunsAfterWithIsolatedHistory(t *testing.T) {
	r, w, _ := fixture(t)
	w.block = make(chan struct{})
	w.started = make(chan struct{}, 2)
	ctx := context.Background()
	must(t, r.Handle(ctx, input(1, -10, 7, 1, "first topic7")))
	<-w.started
	must(t, r.Handle(ctx, input(2, -10, 8, 1, "second topic8")))
	reply, ok := r.Store.Pending()
	if !ok || reply.Route.UpdateID != 2 || reply.Parts[0] != queuedAck {
		t.Fatalf("queued ack missing: %+v", reply)
	}
	w.mu.Lock()
	if calls := w.calls; calls != 1 {
		w.mu.Unlock()
		t.Fatalf("queued started early: %d", calls)
	}
	w.mu.Unlock()
	close(w.block)
	r.Wait()
	w.mu.Lock()
	calls := w.calls
	w.mu.Unlock()
	if calls != 2 {
		t.Fatalf("queued did not run: %d", calls)
	}
	first, err := r.Store.Load(input(0, -10, 7, 1, "").ConversationKey())
	must(t, err)
	second, err := r.Store.Load(input(0, -10, 8, 1, "").ConversationKey())
	must(t, err)
	if len(first.Pairs) != 1 || len(second.Pairs) != 1 {
		t.Fatalf("histories mixed: %+v %+v", first, second)
	}
	if !strings.Contains(first.Pairs[0].User, "first") || !strings.Contains(second.Pairs[0].User, "second") {
		t.Fatalf("histories crossed: %+v %+v", first, second)
	}
}

func TestQueuedGlobalLimitSkipsBusyWorkspace(t *testing.T) {
	r, _, _ := fixture(t)
	blockA := make(chan struct{})
	blockB := make(chan struct{})
	reqs := make(chan llm.Request, 16)
	prov := &selectiveQueueProvider{requests: reqs, blockA: blockA, blockB: blockB}
	r.Providers = func(state.Conversation) (llm.Provider, error) { return prov, nil }
	ctx := context.Background()
	// Fill both slots: A blocks, B blocks.
	must(t, r.Handle(ctx, input(1, -10, 0, 1, "task A")))
	select {
	case <-reqs:
	case <-time.After(3 * time.Second):
		t.Fatal("task A did not start")
	}
	must(t, r.Handle(ctx, input(2, -20, 0, 1, "task B")))
	select {
	case <-reqs:
	case <-time.After(3 * time.Second):
		t.Fatal("task B did not start")
	}
	// Head is blocked by same workspace as A, tail is free workspace C but
	// blocked by global limit.
	must(t, r.Handle(ctx, input(3, -10, 8, 1, "queued same workspace")))
	must(t, r.Handle(ctx, input(4, -30, 0, 1, "queued free workspace")))
	// Finish B: free workspace C must skip the still-blocked head.
	close(blockB)
	var gotFree string
	select {
	case req := <-reqs:
		gotFree = strings.Join(userTexts(req), "|")
	case <-time.After(3 * time.Second):
		t.Fatal("free workspace did not skip blocked head")
	}
	if gotFree != "queued free workspace" {
		t.Fatalf("head-of-line blocked free workspace: %v", gotFree)
	}
	// Finish A: blocked head runs last.
	close(blockA)
	var gotSame string
	select {
	case req := <-reqs:
		gotSame = strings.Join(userTexts(req), "|")
	case <-time.After(3 * time.Second):
		t.Fatal("blocked head did not run")
	}
	if gotSame != "queued same workspace" {
		t.Fatalf("blocked head did not run last: %v", gotSame)
	}
	r.Wait()
}

func TestQueuedCancelRemovesOnlyThatTopic(t *testing.T) {
	r, w, _ := fixture(t)
	w.block = make(chan struct{})
	w.started = make(chan struct{}, 2)
	ctx := context.Background()
	must(t, r.Handle(ctx, input(1, -10, 7, 1, "active")))
	<-w.started
	must(t, r.Handle(ctx, input(2, -10, 8, 1, "queued to cancel")))
	must(t, r.Handle(ctx, input(3, -10, 8, 1, "/cancel")))
	// Drain to the cancel ack (steer acks not present here, only queued + cancel).
	foundCancel := false
	for {
		rep, ok := r.Store.Pending()
		if !ok {
			break
		}
		for _, part := range rep.Parts {
			if strings.Contains(part, "Queued task cancelled") {
				foundCancel = true
			}
		}
		must(t, r.Store.Delivered(rep.ID))
	}
	if !foundCancel {
		t.Fatal("queued cancel ack missing")
	}
	close(w.block)
	r.Wait()
	w.mu.Lock()
	calls := w.calls
	w.mu.Unlock()
	if calls != 1 {
		t.Fatalf("cancelled queued task ran: %d", calls)
	}
	other, err := r.Store.Load(input(0, -10, 8, 1, "").ConversationKey())
	must(t, err)
	if len(other.Pairs) != 0 {
		t.Fatalf("cancelled queued task saved history: %+v", other)
	}
}

func TestQueuedBounds(t *testing.T) {
	r, w, _ := fixture(t)
	w.block = make(chan struct{})
	w.started = make(chan struct{}, 32)
	ctx := context.Background()
	must(t, r.Handle(ctx, input(1, -10, 7, 1, "active")))
	<-w.started
	// Fill queue to capacity with different free workspaces would start
	// immediately (limit 2 allows one more). Occupy second slot first.
	must(t, r.Handle(ctx, input(2, -20, 0, 1, "second slot")))
	<-w.started
	// Now both slots busy: queue maxQueuedTasks. Use allowed chat -30 with
	// distinct topics (same workspace, different conversations) so every
	// message is authorized but blocked by global capacity.
	for i := 0; i < maxQueuedTasks; i++ {
		must(t, r.Handle(ctx, input(int64(10+i), -30, int64(i), 1, "queued")))
	}
	// Next must be rejected, not queued.
	must(t, r.Handle(ctx, input(1000, -30, 99, 1, "one too many")))
	foundBusy := false
	for {
		rep, ok := r.Store.Pending()
		if !ok {
			break
		}
		for _, part := range rep.Parts {
			if strings.Contains(part, "Too many queued") {
				foundBusy = true
			}
		}
		must(t, r.Store.Delivered(rep.ID))
	}
	if !foundBusy {
		t.Fatal("queue bound not enforced")
	}
	close(w.block)
	r.Wait()
}
