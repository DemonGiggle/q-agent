package runtime

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"q-agent/internal/llm"
	"q-agent/internal/platform"
	"q-agent/internal/state"
)

// Same-day preference must be readable before the nightly digest runs.
func TestPendingReadableBeforeDigest(t *testing.T) {
	r, _, _ := digestFixture(t)
	route := input(1, -10, 7, 1, "")
	pipelineSource(t, r, route, "today-1", "user prefers oolong tea in the afternoon")
	history := r.memoryHistory(route, state.Conversation{})
	if len(history) == 0 {
		t.Fatal("no memory injected")
	}
	combined := history[0].Content
	if !strings.Contains(combined, "Unconsolidated") || !strings.Contains(combined, "oolong") {
		t.Fatalf("pending not injected: %q", combined)
	}
}

// The same preference digested three times must stay one note.
func TestDigestDedupsRepeatedPreference(t *testing.T) {
	r, _, _ := digestFixture(t)
	route := input(1, -10, 7, 1, "")
	for _, id := range []string{"a", "b", "c"} {
		pipelineSource(t, r, route, id, "user message suggests they prefer oolong tea")
	}
	p := &memoryProvider{complete: func(_ context.Context, _ llm.Request, _ int) (string, error) {
		return `[{"text":"prefers oolong tea","kind":"preference","keys":["oolong","tea"],"importance":4}]`, nil
	}}
	installMemoryProvider(r, p)
	must(t, r.digestSweep(context.Background()))
	mem, err := r.Store.LoadMemory(route.WorkspaceKey())
	must(t, err)
	if len(mem.Entries) != 1 || len(mem.Pending) != 0 {
		t.Fatalf("duplicate preference: %+v", mem)
	}
}

// /remember pins; /memory shows the pin; /forget <keyword> removes matches.
func TestRememberAndForgetKeywordCommands(t *testing.T) {
	r, _, _ := digestFixture(t)
	must(t, r.Handle(context.Background(), input(1, -10, 0, 1, "/remember prefers oolong tea")))
	r.Wait()
	mem, err := r.Store.LoadMemory(platform.WorkspaceKey("telegram", -10))
	must(t, err)
	if len(mem.Entries) != 1 || !mem.Entries[0].Pinned || mem.Entries[0].Importance != 5 {
		t.Fatalf("remember: %+v", mem.Entries)
	}
	drainDigestReplies(t, r)
	must(t, r.Handle(context.Background(), input(2, -10, 0, 1, "/memory")))
	reply, ok := r.Store.Pending()
	if !ok || !strings.Contains(strings.Join(reply.Parts, "\n"), "pinned") {
		t.Fatalf("memory hides pin: %+v", reply)
	}
	must(t, r.Store.Delivered(reply.ID))
	must(t, r.Handle(context.Background(), input(3, -10, 0, 1, "/forget oolong")))
	r.Wait()
	mem, err = r.Store.LoadMemory(platform.WorkspaceKey("telegram", -10))
	must(t, err)
	if len(mem.Entries) != 0 {
		t.Fatalf("forget keyword: %+v", mem.Entries)
	}
}

// A chat with lots of unconsolidated journal does not wait for midnight.
func TestDaytimeDigestThreshold(t *testing.T) {
	r, _, _ := digestFixture(t)
	p := &memoryProvider{}
	installMemoryProvider(r, p)
	route := input(1, -10, 7, 1, "")
	for i := 0; i < smallDigestMinPending; i++ {
		pipelineSource(t, r, route, fmt.Sprintf("daytime-%d", i), "daytime chatter")
	}
	mem, err := r.Store.LoadMemory(route.WorkspaceKey())
	must(t, err)
	if len(mem.Pending) != smallDigestMinPending {
		t.Fatalf("pending=%d", len(mem.Pending))
	}
	// No nightly sweep: the minute-tick worker still picks up the backlog.
	must(t, r.digestWork(context.Background(), false))
	mem, err = r.Store.LoadMemory(route.WorkspaceKey())
	must(t, err)
	if len(mem.Pending) != 0 || len(mem.Entries) == 0 {
		t.Fatalf("daytime digest did not run: pending=%d entries=%d", len(mem.Pending), len(mem.Entries))
	}
}

// Structured JSON distill output merges rather than appending duplicates.
func TestStructuredDistillMerges(t *testing.T) {
	existing := []state.MemoryEntry{{Date: "2026-10-01", Text: "prefers oolong tea", Kind: "preference", Keys: []string{"oolong", "tea"}, Importance: 4}}
	fresh := parseDistilledNotes(`[{"text":"prefers oolong tea","kind":"preference","keys":["oolong","tea"],"importance":4}]`, "2026-11-03")
	merged := mergeMemoryEntries(existing, fresh)
	if len(merged) != 1 {
		t.Fatalf("duplicate: %+v", merged)
	}
	other := parseDistilledNotes(`[{"text":"deploys with acme v2","kind":"project","keys":["acme"],"importance":5}]`, "2026-11-03")
	merged = mergeMemoryEntries(merged, other)
	if len(merged) != 2 {
		t.Fatalf("distinct dropped: %+v", merged)
	}
}
