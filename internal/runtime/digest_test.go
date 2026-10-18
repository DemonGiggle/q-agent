package runtime

import (
	"context"
	"strings"
	"testing"

	"q-agent/internal/platform"
	"q-agent/internal/state"
)

func digestFixture(t *testing.T) (*Runtime, *testWorkspace, *testProvider) {
	t.Helper()
	r, w, p := fixture(t)
	r.Config.Memory.MaxBytes = 8192
	r.Config.Memory.MaxContextBytes = 2000
	return r, w, p
}

func drainDigestReplies(t *testing.T, r *Runtime) {
	t.Helper()
	for reply, ok := r.Store.Pending(); ok; reply, ok = r.Store.Pending() {
		if err := r.Store.Delivered(reply.ID); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDigestSweepStoresMemoryAndNotifies(t *testing.T) {
	r, _, _ := digestFixture(t)
	must(t, r.Handle(context.Background(), input(1, 10, 0, 1, "remember the sky is blue")))
	r.Wait()
	must(t, r.Handle(context.Background(), input(2, 10, 7, 1, "and grass is green")))
	r.Wait()
	drainDigestReplies(t, r)
	must(t, r.digestSweep(context.Background()))
	mem, err := r.Store.LoadMemory(platform.WorkspaceKey("telegram", 10))
	must(t, err)
	if len(mem.Entries) != 1 {
		t.Fatalf("entries: %+v", mem.Entries)
	}
	if mem.Digested[memKey(10, 0)] != 1 || mem.Digested[memKey(10, 7)] != 1 {
		t.Fatalf("watermarks: %+v", mem.Digested)
	}
	reply, ok := r.Store.Pending()
	if !ok {
		t.Fatal("missing digest notification")
	}
	if reply.Route.ChatID != 10 || reply.Route.TopicID != 0 {
		t.Fatalf("wrong notify route: %+v", reply.Route)
	}
	text := strings.Join(reply.Parts, "\n")
	if !strings.Contains(text, "Midnight digest") || !strings.Contains(text, "answer") {
		t.Fatalf("notification missing preamble/summary: %q", text)
	}
}

func memKey(chat, topic int64) string {
	return platform.WorkspaceKey("telegram", chat) + ":" + itoa(int(topic))
}

func TestDigestSweepSkipsBusyChats(t *testing.T) {
	r, _, _ := digestFixture(t)
	must(t, r.Handle(context.Background(), input(1, 10, 0, 1, "remember this")))
	r.Wait()
	drainDigestReplies(t, r)
	r.mu.Lock()
	r.active[platform.WorkspaceKey("telegram", 10)] = task{topic: 0}
	r.mu.Unlock()
	must(t, r.digestSweep(context.Background()))
	mem, err := r.Store.LoadMemory(platform.WorkspaceKey("telegram", 10))
	must(t, err)
	if len(mem.Entries) != 0 {
		t.Fatalf("busy chat digested: %+v", mem.Entries)
	}
	if _, ok := r.Store.Pending(); ok {
		t.Fatal("busy chat notified")
	}
	r.mu.Lock()
	delete(r.active, platform.WorkspaceKey("telegram", 10))
	r.mu.Unlock()
}

func TestMemoryEngagedInChat(t *testing.T) {
	r, _, p := digestFixture(t)
	key := platform.WorkspaceKey("telegram", 10)
	must(t, r.Store.SaveMemory(key, state.ChatMemory{Entries: []state.MemoryEntry{{Date: "2026-10-17", Text: "user prefers concise answers"}}}))
	must(t, r.Handle(context.Background(), input(1, 10, 0, 1, "go")))
	r.Wait()
	if len(p.requests) == 0 {
		t.Fatal("no provider request")
	}
	msgs := p.requests[0].Messages
	found := false
	for _, msg := range msgs {
		if msg.Role == "system" && strings.Contains(msg.Content, "concise answers") {
			found = true
		}
	}
	if !found {
		t.Fatalf("memory not injected: %+v", msgs)
	}
}

func TestMemoryCommands(t *testing.T) {
	r, _, _ := digestFixture(t)
	key := platform.WorkspaceKey("telegram", 10)
	must(t, r.Store.SaveMemory(key, state.ChatMemory{Entries: []state.MemoryEntry{{Date: "2026-10-17", Text: "likes tea"}}}))
	must(t, r.Handle(context.Background(), input(1, 10, 0, 1, "/memory")))
	reply, ok := r.Store.Pending()
	if !ok || !strings.Contains(strings.Join(reply.Parts, "\n"), "likes tea") {
		t.Fatalf("memory not shown: %+v", reply)
	}
	must(t, r.Store.Delivered(reply.ID))
	must(t, r.Handle(context.Background(), input(2, 10, 0, 1, "/forget")))
	mem, err := r.Store.LoadMemory(key)
	must(t, err)
	if len(mem.Entries) != 0 {
		t.Fatalf("memory not cleared: %+v", mem.Entries)
	}
	reply, ok = r.Store.Pending()
	if !ok || !strings.Contains(strings.Join(reply.Parts, "\n"), "cleared") {
		t.Fatalf("forget not acknowledged: %+v", reply)
	}
}
