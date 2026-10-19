package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"q-agent/internal/config"
	"q-agent/internal/llm"
	"q-agent/internal/platform"
	"q-agent/internal/redaction"
	"q-agent/internal/state"
)

type memoryProvider struct {
	mu       sync.Mutex
	requests []llm.Request
	complete func(context.Context, llm.Request, int) (string, error)
}

func (p *memoryProvider) Name() string { return "ollama" }
func (p *memoryProvider) Complete(ctx context.Context, req llm.Request, _ llm.StreamCallback) (llm.Response, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	n := len(p.requests)
	p.mu.Unlock()
	text := "- durable note"
	var err error
	if p.complete != nil {
		text, err = p.complete(ctx, req, n)
	}
	return llm.Response{Message: llm.Message{Role: "assistant", Content: text}}, err
}
func (p *memoryProvider) count() int { p.mu.Lock(); defer p.mu.Unlock(); return len(p.requests) }
func installMemoryProvider(r *Runtime, p *memoryProvider) {
	r.Providers = func(state.Conversation) (llm.Provider, error) { return p, nil }
}
func pipelineSource(t *testing.T, r *Runtime, route platform.Message, id, text string) {
	t.Helper()
	must(t, r.Store.RecordChat(route, time.Now()))
	c, err := r.Store.Load(route.ConversationKey())
	must(t, err)
	c.Pairs = append(c.Pairs, state.Pair{User: text, Steers: []string{"ordered steer one", "ordered steer two"}, Answer: "final answer"})
	// Old sources have no age expiry.
	must(t, r.Store.CommitExchange(route, id, c, time.Now().Add(-365*24*time.Hour)))
}

func TestDigestBatchesRetainUnprocessedSources(t *testing.T) {
	r, _, _ := digestFixture(t)
	p := &memoryProvider{}
	installMemoryProvider(r, p)
	route := input(1, 10, 7, 1, "")
	key := route.WorkspaceKey()
	for i := 1; i <= 4; i++ {
		pipelineSource(t, r, route, fmt.Sprint(i), fmt.Sprintf("exchange%d ", i)+strings.Repeat("界🙂", 1600))
	}
	must(t, r.digestChat(context.Background(), key, state.ChatRecord{Platform: "telegram", ChatID: 10, Owner: 1}))
	mem, err := r.Store.LoadMemory(key)
	must(t, err)
	if mem.Cursor != 1 || len(mem.Pending) != 3 {
		t.Fatalf("unprocessed sources acknowledged: %+v", mem)
	}
	must(t, r.digestSweep(context.Background()))
	mem, err = r.Store.LoadMemory(key)
	must(t, err)
	if mem.Cursor != 4 || len(mem.Pending) != 0 || p.count() != 4 {
		t.Fatalf("multi-batch digest: cursor %d pending %d calls %d", mem.Cursor, len(mem.Pending), p.count())
	}
	for i, req := range p.requests {
		text := req.Messages[1].Content
		if len(text) > digestTranscriptCap || !utf8.ValidString(text) || !strings.Contains(text, fmt.Sprintf("exchange%d", i+1)) || !strings.Contains(text, "steer: ordered steer one\nsteer: ordered steer two\nassistant: final answer") || len(req.Tools) != 0 {
			t.Fatalf("invalid transcript %d, %d bytes", i, len(text))
		}
	}
}

func TestOversizedSourceFragmentsCommitAsOneGroup(t *testing.T) {
	r, _, _ := digestFixture(t)
	route := input(1, 10, 7, 1, "")
	key := route.WorkspaceKey()
	pipelineSource(t, r, route, "large", "HEAD"+strings.Repeat("界🙂\n", 11000)+"TAIL")
	pipelineSource(t, r, route, "next", "unprocessed tail source")
	fail := true
	p := &memoryProvider{complete: func(_ context.Context, _ llm.Request, n int) (string, error) {
		if fail && n == 2 {
			return "", errors.New("fragment failed")
		}
		return fmt.Sprintf("- fragment note %d", n), nil
	}}
	installMemoryProvider(r, p)
	rec := state.ChatRecord{Platform: "telegram", ChatID: 10, Owner: 1}
	if err := r.digestChat(context.Background(), key, rec); err == nil {
		t.Fatalf("fragment failure accepted: calls=%d", p.count())
	}
	mem, _ := r.Store.LoadMemory(key)
	if len(mem.Pending) != 2 || mem.Cursor != 0 || len(mem.Entries) != 0 {
		t.Fatal("partial fragment group committed")
	}
	fail = false
	must(t, r.digestChat(context.Background(), key, rec))
	mem, _ = r.Store.LoadMemory(key)
	if mem.Cursor != 1 || len(mem.Pending) != 1 || mem.Pending[0].ID != "next" || len(mem.Entries) != 1 {
		t.Fatalf("fragment ack: %+v", mem)
	}
	foundTail := false
	for _, req := range p.requests {
		text := req.Messages[1].Content
		if len(text) > digestTranscriptCap || !utf8.ValidString(text) || !strings.Contains(text, "fragment ") {
			t.Fatal("invalid fragment")
		}
		if strings.Contains(text, "TAIL") {
			foundTail = true
		}
	}
	if !foundTail {
		t.Fatal("last fragment was lost")
	}
	calls := p.count()
	// Failed delivery remains queued; no source is distilled again after commit.
	reply, ok := r.Store.Pending()
	if !ok {
		t.Fatal("notification missing")
	}
	must(t, r.Store.DeferReply(reply.ID))
	must(t, r.digestChat(context.Background(), key, rec)) // only the next source
	if p.count() != calls+1 {
		t.Fatal("committed fragments distilled again")
	}
}

func TestDigestRejectsForgetDuringModelAndPreservesConcurrentAppend(t *testing.T) {
	for _, forget := range []bool{false, true} {
		t.Run(fmt.Sprint(forget), func(t *testing.T) {
			r, _, _ := digestFixture(t)
			route := input(1, 10, 0, 1, "")
			key := route.WorkspaceKey()
			pipelineSource(t, r, route, "1", "first source")
			started, release := make(chan struct{}), make(chan struct{})
			p := &memoryProvider{complete: func(ctx context.Context, _ llm.Request, _ int) (string, error) {
				close(started)
				select {
				case <-release:
					return "- first note", nil
				case <-ctx.Done():
					return "", ctx.Err()
				}
			}}
			installMemoryProvider(r, p)
			done := make(chan error, 1)
			go func() {
				done <- r.digestChat(context.Background(), key, state.ChatRecord{Platform: "telegram", ChatID: 10, Owner: 1})
			}()
			<-started
			if forget {
				must(t, r.Store.ClearMemory(key))
			}
			pipelineSource(t, r, route, "2", "completed during digest")
			close(release)
			err := <-done
			if forget {
				if !errors.Is(err, state.ErrStaleDigest) {
					t.Fatalf("forget accepted old model result: %v", err)
				}
			} else {
				must(t, err)
			}
			mem, _ := r.Store.LoadMemory(key)
			if len(mem.Pending) != 1 || mem.Pending[0].ID != "2" || (forget && len(mem.Entries) != 0) {
				t.Fatalf("concurrent append lost: %+v", mem)
			}
		})
	}
}

func TestCompletionAfterForgetAndResetsRetainSources(t *testing.T) {
	r, _, _ := digestFixture(t)
	route := input(1, 10, 7, 1, "")
	key := route.WorkspaceKey()
	pipelineSource(t, r, route, "before", "pre-forget source")
	started, release := make(chan struct{}), make(chan struct{})
	p := &memoryProvider{complete: func(ctx context.Context, _ llm.Request, _ int) (string, error) {
		close(started)
		select {
		case <-release:
			return "completed after forget", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}}
	installMemoryProvider(r, p)
	must(t, r.Handle(context.Background(), input(1, 10, 7, 1, "running task")))
	<-started
	must(t, r.Handle(context.Background(), input(2, 10, 8, 1, "/forget")))
	close(release)
	r.Wait()
	mem, _ := r.Store.LoadMemory(key)
	if len(mem.Pending) != 1 || mem.Pending[0].ID != "update:telegram:1" || !strings.Contains(mem.Pending[0].Transcript, "completed after forget") {
		t.Fatalf("completion eligibility: %+v", mem)
	}
	must(t, r.Handle(context.Background(), input(3, 10, 7, 1, "/new")))
	must(t, r.Handle(context.Background(), input(4, 10, 7, 1, "/model changed-model")))
	r.Config.Providers = map[string]config.Provider{"openai": {Name: "openai", Model: "another-model"}}
	must(t, r.Handle(context.Background(), input(5, 10, 7, 1, "/provider openai")))
	mem, _ = r.Store.LoadMemory(key)
	if len(mem.Pending) != 1 {
		t.Fatal("history/model/provider reset discarded source")
	}
	history, err := r.loadModelConversation(route)
	must(t, err)
	if len(history.Pairs) != 0 || history.Model != "another-model" {
		t.Fatalf("history reset did not occur: %+v", history)
	}
}

func TestDigestRetriesAndBackoffSurviveRestart(t *testing.T) {
	r, _, _ := digestFixture(t)
	route := input(1, 10, 0, 1, "")
	key := route.WorkspaceKey()
	pipelineSource(t, r, route, "1", "retry me")
	failing := true
	p := &memoryProvider{complete: func(_ context.Context, _ llm.Request, _ int) (string, error) {
		if failing {
			return "", errors.New("model unavailable")
		}
		return "- retry succeeded", nil
	}}
	installMemoryProvider(r, p)
	before := time.Now()
	must(t, r.digestSweep(context.Background()))
	mem, _ := r.Store.LoadMemory(key)
	if mem.Retry.Attempts != 1 || !mem.Retry.Work || mem.Retry.NextAt.Before(before.Add(time.Minute)) || len(mem.Pending) != 1 {
		t.Fatalf("retry state: %+v", mem.Retry)
	}
	must(t, r.digestSweep(context.Background()))
	if p.count() != 1 {
		t.Fatal("backoff bypassed by new sweep")
	}
	reopened, err := state.Open(r.Config.StateDir, nil)
	must(t, err)
	r.Store = reopened
	r.mu.Lock()
	r.digestRetries = nil
	r.mu.Unlock()
	must(t, r.digestWork(context.Background(), false))
	if p.count() != 1 {
		t.Fatal("restart bypassed backoff")
	}
	mem.Retry.NextAt = time.Now().Add(-time.Second)
	must(t, r.Store.RetryMemory(key, mem.Epoch, mem.Retry))
	failing = false
	must(t, r.digestWork(context.Background(), false))
	mem, _ = r.Store.LoadMemory(key)
	if len(mem.Pending) != 0 || mem.Retry.Attempts != 0 || p.count() != 2 {
		t.Fatalf("retry did not complete: %+v", mem)
	}
	expected := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 32 * time.Minute, time.Hour, time.Hour}
	for i, want := range expected {
		if got := digestRetryDelay(i + 1); got != want {
			t.Fatalf("attempt %d: %s want %s", i+1, got, want)
		}
	}
}

func TestDigestStorageFailureHasRuntimeRetry(t *testing.T) {
	r, _, _ := digestFixture(t)
	route := input(1, 10, 0, 1, "")
	key := route.WorkspaceKey()
	pipelineSource(t, r, route, "1", "storage retry")
	path := filepath.Join(r.Config.StateDir, "memory", key+".json")
	p := &memoryProvider{complete: func(_ context.Context, _ llm.Request, n int) (string, error) {
		if n == 1 {
			if err := os.Rename(path, path+".saved"); err != nil {
				return "", err
			}
			if err := os.Mkdir(path, 0700); err != nil {
				return "", err
			}
		}
		return "- stored after retry", nil
	}}
	installMemoryProvider(r, p)
	must(t, r.digestSweep(context.Background()))
	r.mu.Lock()
	retry := r.digestRetries[key]
	r.mu.Unlock()
	if retry.retry.Attempts != 1 || !retry.retry.Work {
		t.Fatal("storage error was not scheduled")
	}
	must(t, r.digestWork(context.Background(), false))
	r.mu.Lock()
	unchanged := r.digestRetries[key]
	r.mu.Unlock()
	if !unchanged.retry.NextAt.Equal(retry.retry.NextAt) {
		t.Fatal("persistent read failure reset the backoff")
	}
	retry.retry.NextAt = time.Now().Add(-time.Second)
	r.mu.Lock()
	r.digestRetries[key] = retry
	r.mu.Unlock()
	must(t, r.digestWork(context.Background(), false))
	r.mu.Lock()
	retry = r.digestRetries[key]
	r.mu.Unlock()
	if retry.retry.Attempts != 2 || time.Until(retry.retry.NextAt) < 119*time.Second {
		t.Fatalf("read retry backoff: %+v", retry)
	}
	must(t, os.Remove(path))
	must(t, os.Rename(path+".saved", path))
	must(t, r.digestWork(context.Background(), false))
	if p.count() != 1 {
		t.Fatal("storage backoff bypassed")
	}
	retry.retry.NextAt = time.Now().Add(-time.Second)
	r.mu.Lock()
	r.digestRetries[key] = retry
	r.mu.Unlock()
	must(t, r.digestWork(context.Background(), false))
	mem, _ := r.Store.LoadMemory(key)
	if len(mem.Pending) != 0 || len(mem.Entries) != 1 || p.count() != 2 {
		t.Fatalf("storage retry failed: %+v", mem)
	}
}

func TestBusyStartupDigestWakesAfterFirstTaskCompletes(t *testing.T) {
	r, _, _ := digestFixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	p := &memoryProvider{complete: func(ctx context.Context, _ llm.Request, n int) (string, error) {
		if n == 1 {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		return "- caught up", nil
	}}
	installMemoryProvider(r, p)
	must(t, r.Handle(context.Background(), input(1, 10, 7, 1, "first running task")))
	<-started
	must(t, r.StartScheduler(context.Background()))
	key := platform.WorkspaceKey("telegram", 10)
	waitMemory(t, r, key, func(mem state.ChatMemory) bool { return mem.Retry.Deferred })
	close(release)
	r.Wait()
	mem := waitMemory(t, r, key, func(mem state.ChatMemory) bool { return mem.Cursor == 1 && len(mem.Pending) == 0 })
	if len(mem.Entries) != 1 || p.count() != 2 {
		t.Fatalf("busy catch-up: %+v, calls %d", mem, p.count())
	}
}

func waitMemory(t *testing.T, r *Runtime, key string, ready func(state.ChatMemory) bool) state.ChatMemory {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		mem, err := r.Store.LoadMemory(key)
		if err == nil && ready(mem) {
			return mem
		}
		select {
		case <-timer.C:
			t.Fatalf("memory condition timed out: %+v, %v", mem, err)
		case <-ticker.C:
		}
	}
}

func TestCompactionFailuresStillEnforceHardBudget(t *testing.T) {
	for _, mode := range []string{"error", "empty", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			r, _, _ := digestFixture(t)
			r.Config.Memory.MaxBytes = 17
			route := input(1, 10, 0, 1, "")
			key := route.WorkspaceKey()
			_, err := r.Store.AppendMemory(key, "old", "old durable note", 8192)
			must(t, err)
			pipelineSource(t, r, route, "1", "new source")
			p := &memoryProvider{complete: func(_ context.Context, _ llm.Request, n int) (string, error) {
				if n == 1 {
					return strings.Repeat("新🙂", 50), nil
				}
				switch mode {
				case "error":
					return "", errors.New("compaction unavailable")
				case "empty":
					return "", nil
				default:
					return strings.Repeat("旧🙂", 500), nil
				}
			}}
			installMemoryProvider(r, p)
			must(t, r.digestSweep(context.Background()))
			mem, _ := r.Store.LoadMemory(key)
			if mem.Bytes() > 17 || len(mem.Entries) != 1 || !utf8.ValidString(mem.Entries[0].Text) || mem.Loss.DroppedNoteBytes == 0 || len(mem.Pending) != 0 {
				t.Fatalf("unbounded failed compaction: %+v", mem)
			}
		})
	}
}

func TestMemoryContextCompleteFormatWithinUTF8Budget(t *testing.T) {
	mem := state.ChatMemory{Entries: []state.MemoryEntry{{Date: "2026-10-18", Text: "old note"}, {Date: "2026-10-19", Text: strings.Repeat("新🙂", 50)}}}
	for limit := 1; limit < 240; limit++ {
		text := memoryText(mem, limit)
		if len(text) > limit || !utf8.ValidString(text) {
			t.Fatalf("budget %d: %q", limit, text)
		}
		if text != "" && (!strings.HasPrefix(text, memoryHeading) || !strings.Contains(text, "2026-10-19")) {
			t.Fatalf("minimum format/recent priority %d: %q", limit, text)
		}
	}
	r, _, _ := digestFixture(t)
	disabled := false
	policy, err := redaction.New(redaction.Config{Persistence: &disabled}, []string{"x"})
	must(t, err)
	r.Options.Redaction = policy
	r.Store, err = state.Open(t.TempDir(), policy)
	must(t, err)
	route := input(1, 10, 0, 1, "")
	_, err = r.Store.AppendMemory(route.WorkspaceKey(), "2026-10-19", strings.Repeat("x界🙂", 30), 8192)
	must(t, err)
	for limit := 1; limit < 240; limit++ {
		r.Config.Memory.MaxContextBytes = limit
		history := r.memoryHistory(route, state.Conversation{})
		if len(history) > 0 && (len(history[0].Content) > limit || !utf8.ValidString(history[0].Content) || strings.Contains(history[0].Content, "x")) {
			t.Fatalf("remote expansion violated budget %d: %+v", limit, history)
		}
	}
}

func TestDigestFairnessAndNotificationRetryWithoutDistillation(t *testing.T) {
	r, _, _ := digestFixture(t)
	p := &memoryProvider{}
	installMemoryProvider(r, p)
	for _, chat := range []int64{10, 20} {
		for i := 1; i <= 2; i++ {
			pipelineSource(t, r, input(1, chat, 0, 1, ""), fmt.Sprintf("%d:%d", chat, i), fmt.Sprintf("chat%d exchange%d ", chat, i)+strings.Repeat("a", 11000))
		}
	}
	must(t, r.digestSweep(context.Background()))
	if p.count() != 4 {
		t.Fatalf("calls=%d", p.count())
	}
	firstChat := strings.Contains(p.requests[0].Messages[1].Content, "chat10")
	if strings.Contains(p.requests[1].Messages[1].Content, "chat10") == firstChat || strings.Contains(p.requests[2].Messages[1].Content, "chat10") != firstChat {
		t.Fatal("one chat monopolized batch worker")
	}
	reply, ok := r.Store.Pending()
	if !ok {
		t.Fatal("missing notification")
	}
	must(t, r.Store.DeferReply(reply.ID)) // simulate failed network delivery
	reopened, err := state.Open(r.Config.StateDir, nil)
	must(t, err)
	r.Store = reopened
	must(t, r.digestSweep(context.Background()))
	if p.count() != 4 {
		t.Fatal("notification retry repeated distillation")
	}
	notices := 0
	for reply, ok := r.Store.Pending(); ok; reply, ok = r.Store.Pending() {
		notices++
		must(t, r.Store.Delivered(reply.ID))
	}
	if notices != 4 {
		t.Fatalf("batch notifications=%d", notices)
	}
}

func TestRuntimeCapturesExchangesThirteenAndFourteen(t *testing.T) {
	r, _, _ := digestFixture(t)
	for i := int64(1); i <= 14; i++ {
		must(t, r.Handle(context.Background(), input(i, 10, 0, 1, fmt.Sprintf("exchange %d", i))))
		r.Wait()
	}
	key := platform.WorkspaceKey("telegram", 10)
	mem, err := r.Store.LoadMemory(key)
	must(t, err)
	if len(mem.Pending) != 14 || !strings.Contains(mem.Pending[12].Transcript, "exchange 13") || !strings.Contains(mem.Pending[13].Transcript, "exchange 14") {
		t.Fatal("completion capture depends on bounded history")
	}
	must(t, r.digestSweep(context.Background()))
	mem, err = r.Store.LoadMemory(key)
	must(t, err)
	if mem.Cursor != 14 || len(mem.Pending) != 0 {
		t.Fatal("late exchanges were not digested")
	}
}

func TestDigestRemoteExpansionAndEmptySummary(t *testing.T) {
	r, _, _ := digestFixture(t)
	disabled := false
	policy, err := redaction.New(redaction.Config{Persistence: &disabled}, []string{"x"})
	must(t, err)
	r.Store, err = state.Open(t.TempDir(), policy)
	must(t, err)
	r.Options.Redaction = policy
	route := input(1, 10, 0, 1, "")
	key := route.WorkspaceKey()
	pipelineSource(t, r, route, "1", strings.Repeat("x", 7000))
	empty := true
	p := &memoryProvider{complete: func(_ context.Context, _ llm.Request, _ int) (string, error) {
		if empty {
			return "", nil
		}
		return "- durable", nil
	}}
	installMemoryProvider(r, p)
	rec := state.ChatRecord{Platform: "telegram", ChatID: 10, Owner: 1}
	if err := r.digestChat(context.Background(), key, rec); err == nil {
		t.Fatal("empty summary acknowledged")
	}
	mem, _ := r.Store.LoadMemory(key)
	if len(mem.Pending) != 1 || mem.Cursor != 0 {
		t.Fatal("empty summary lost source")
	}
	empty = false
	must(t, r.digestChat(context.Background(), key, rec))
	if p.count() < 4 {
		t.Fatal("remote redaction expansion did not fragment")
	}
	for _, req := range p.requests {
		if len(req.Messages[1].Content) > digestTranscriptCap || strings.Contains(req.Messages[1].Content, "x") {
			t.Fatal("redaction exceeded batch budget or exposed source")
		}
	}
}

func TestMemoryContextRedactedFormattingMustFit(t *testing.T) {
	r, _, _ := digestFixture(t)
	disabled := false
	policy, err := redaction.New(redaction.Config{Persistence: &disabled}, []string{"-", "memory"})
	must(t, err)
	r.Options.Redaction = policy
	route := input(1, 10, 0, 1, "")
	_, err = r.Store.AppendMemory(route.WorkspaceKey(), "2026-10-19", "🙂 recent note", 8192)
	must(t, err)
	minimum := len(policy.Text(redaction.Remote, memoryHeading+"- [2026-10-19] 🙂"))
	for limit := 1; limit < minimum+40; limit++ {
		r.Config.Memory.MaxContextBytes = limit
		history := r.memoryHistory(route, state.Conversation{})
		if limit < minimum && len(history) != 0 {
			t.Fatalf("incomplete minimum format at %d: %+v", limit, history)
		}
		if len(history) > 0 && (len(history[0].Content) > limit || !utf8.ValidString(history[0].Content)) {
			t.Fatalf("redacted format exceeded %d", limit)
		}
	}
}

func TestMemoryShowsPendingAndLossWithoutNotes(t *testing.T) {
	r, _, _ := digestFixture(t)
	var err error
	r.Store, err = state.Open(t.TempDir(), nil, state.MemoryLimits{MaxPendingBytes: 500})
	must(t, err)
	route := input(1, 10, 7, 1, "")
	for i := 1; i <= 4; i++ {
		pipelineSource(t, r, route, fmt.Sprint(i), strings.Repeat("a", 150))
	}
	drainDigestReplies(t, r)
	must(t, r.Handle(context.Background(), input(1, 10, 8, 1, "/memory")))
	reply, ok := r.Store.Pending()
	if !ok {
		t.Fatal("missing /memory reply")
	}
	text := strings.Join(reply.Parts, "\n")
	for _, want := range []string{"No long-term memory", "Pending:", "Oldest pending age:", "Retry:", "evicted sources"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %s", want, text)
		}
	}
}

func TestNightlyDigestRetriesSourcesAfterConcurrentEviction(t *testing.T) {
	r, _, _ := digestFixture(t)
	var err error
	r.Store, err = state.Open(t.TempDir(), nil, state.MemoryLimits{MaxPendingBytes: 800})
	must(t, err)
	route := input(1, 10, 0, 1, "")
	key := route.WorkspaceKey()
	pipelineSource(t, r, route, "initial", strings.Repeat("a", 180))
	p := &memoryProvider{complete: func(_ context.Context, _ llm.Request, n int) (string, error) {
		if n == 1 {
			for i := 1; i <= 4; i++ {
				pipelineSource(t, r, route, fmt.Sprint(i), strings.Repeat("b", 180))
			}
		}
		return "- retained source", nil
	}}
	installMemoryProvider(r, p)
	must(t, r.digestSweep(context.Background()))
	mem, err := r.Store.LoadMemory(key)
	must(t, err)
	if len(mem.Pending) != 0 || mem.Cursor != 5 || mem.Loss.LostEvents == 0 || p.count() < 2 {
		t.Fatalf("eviction abandoned nightly work: %+v, calls=%d", mem, p.count())
	}
}

func TestEvictionOfAllSourcesClearsObsoleteRetryState(t *testing.T) {
	r, _, _ := digestFixture(t)
	var err error
	r.Store, err = state.Open(t.TempDir(), nil, state.MemoryLimits{MaxPendingBytes: 800})
	must(t, err)
	route := input(1, 10, 0, 1, "")
	key := route.WorkspaceKey()
	pipelineSource(t, r, route, "1", "will fail distillation")
	p := &memoryProvider{complete: func(_ context.Context, _ llm.Request, _ int) (string, error) { return "", errors.New("unavailable") }}
	installMemoryProvider(r, p)
	must(t, r.digestSweep(context.Background()))
	pipelineSource(t, r, route, "2", strings.Repeat("b\n", 1000)) // exceeds the entire journal capacity
	must(t, r.digestWork(context.Background(), false))
	mem, err := r.Store.LoadMemory(key)
	must(t, err)
	if len(mem.Pending) != 0 || mem.Retry != (state.MemoryRetry{}) || p.count() != 1 {
		t.Fatalf("obsolete retry remained: %+v", mem.Retry)
	}
}

func TestDisabledMemoryRetainsJournalForReenable(t *testing.T) {
	r, _, _ := digestFixture(t)
	p := &memoryProvider{}
	installMemoryProvider(r, p)
	route := input(1, 10, 0, 1, "")
	key := route.WorkspaceKey()
	pipelineSource(t, r, route, "1", "retained source")
	r.Config.Memory.Enabled = false
	must(t, r.digestSweep(context.Background()))
	mem, err := r.Store.LoadMemory(key)
	must(t, err)
	if len(mem.Pending) != 1 || p.count() != 0 {
		t.Fatalf("disabled sweep digested: %+v calls=%d", mem, p.count())
	}
	if got := r.memoryShow(route); !strings.Contains(got, "Pending:") {
		t.Fatalf("/memory unavailable while disabled: %q", got)
	}
	r.Config.Memory.Enabled = true
	must(t, r.digestSweep(context.Background()))
	mem, err = r.Store.LoadMemory(key)
	must(t, err)
	if len(mem.Pending) != 0 || len(mem.Entries) != 1 {
		t.Fatalf("re-enable did not digest retained: %+v", mem)
	}
}
