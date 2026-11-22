package runtime

import (
	"context"
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"q-agent/internal/learning"
	"q-agent/internal/llm"
	"q-agent/internal/localtools"
	"q-agent/internal/state"
)

type nightSearch func(context.Context, string, int) ([]learning.SearchHit, learning.Budget, error)

func (f nightSearch) Search(ctx context.Context, text string, limit int) ([]learning.SearchHit, learning.Budget, error) {
	return f(ctx, text, limit)
}

type nightFetch func(context.Context, string, int64) (learning.FetchedDocument, learning.Budget, error)

func (f nightFetch) Fetch(ctx context.Context, uri string, limit int64) (learning.FetchedDocument, learning.Budget, error) {
	return f(ctx, uri, limit)
}

func TestLearningNightResearchReachesChatRecall(t *testing.T) {
	r, _, provider := fixture(t)
	ctx := context.Background()
	cfg := learning.DefaultConfig()
	cfg.Enabled = true
	cfg.Recall.ChatEnabled = true
	cfg.Schedule.Enabled = true
	cfg.Schedule.TimeZone = "Asia/Taipei"
	cfg.Schedule.WindowStart, cfg.Schedule.WindowEnd = "23:00", "03:00"
	cfg.Permissions.AllowDomains = []string{localtools.LearningSearchDomain, "example.org", "example.net"}
	cfg.Permissions.IncludePrivateChats = true
	r.Learning = cfg
	path := filepath.Join(r.Config.StateDir, "learning", "policy.json")
	if err := state.Atomic(path, cfg); err != nil {
		t.Fatal(err)
	}
	c := state.NewLearningCoordinator(r.Config.StateDir)
	const question = "Historical regency appointments"
	allow := func(string) bool { return true }
	before := time.Date(2026, 11, 22, 14, 0, 0, 0, time.UTC)
	inside := before.Add(2 * time.Hour)
	route := input(1, 1, 0, 1, "")
	if err := r.Store.CommitExchange(route, "update:telegram:1", state.Conversation{Pairs: []state.Pair{{
		User: "Can you explain the regency appointments from our earlier conversation?",
	}}}, before.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	topic := int64(0)
	scope := learning.Scope{Platform: "telegram", ChatID: 1, TopicID: &topic, Visibility: learning.PrivateChat}
	repo := state.NewLearningRepository(r.Config.StateDir, nil)
	entries, err := repo.ConversationFrontier(ctx, r.learningConversationReader(route, cfg.Permissions, nil),
		learning.ConversationQuery{Scope: scope, From: before.Add(-time.Hour), Until: before}, before, allow)
	if err != nil || len(entries) != 1 {
		t.Fatalf("conversation frontier = %+v, %v", entries, err)
	}
	handoff, err := c.QueueFrontier(ctx, cfg, learning.Partition{Scope: &scope}, entries,
		entries[0].Question.ID, question, before, allow)
	if err != nil {
		t.Fatal(err)
	}
	id := handoff.RunID
	const first = "https://example.org/records"
	const second = "https://example.net/archives"
	searches, fetches := 0, 0
	search := nightSearch(func(_ context.Context, query string, _ int) ([]learning.SearchHit, learning.Budget, error) {
		searches++
		if query != question {
			t.Fatalf("search did not receive the approved public text: %q", query)
		}
		return []learning.SearchHit{{URI: first}, {URI: second}}, learning.Budget{Requests: 1}, nil
	})
	fetch := nightFetch(func(_ context.Context, uri string, _ int64) (learning.FetchedDocument, learning.Budget, error) {
		fetches++
		text := "The historical regency appointments are preserved in court records."
		if uri == second {
			text = "The archives summarize historical appointments of regency staff."
		}
		now := time.Now().UTC()
		return learning.FetchedDocument{MediaType: "text/plain", Body: []byte(text), Source: learning.Source{
			Record: learning.Record{SchemaVersion: learning.SchemaVersion, CreatedAt: now, UpdatedAt: now},
			ID:     fmt.Sprintf("%x", sha256.Sum256([]byte(uri))), CanonicalURI: uri, FetchedAt: now,
			ContentHash: fmt.Sprintf("%x", sha256.Sum256([]byte(text))), TrustClass: "public_reference",
		}}, learning.Budget{Requests: 2, Documents: 1}, nil
	})
	tick := func(at time.Time) {
		t.Helper()
		if err := r.runNightResearch(ctx, at, search, fetch); err != nil {
			t.Fatal(err)
		}
	}
	tick(inside)
	if searches != 0 {
		t.Fatal("an ordinary queued run was picked up without overnight approval")
	}
	if err := c.ScheduleResearch(ctx, cfg, id, allow); err != nil {
		t.Fatal(err)
	}
	tick(before)
	if err := r.Store.RecordChat(input(1, 1, 0, 1, ""), inside.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	tick(inside)
	disabled := cfg
	disabled.Schedule.Enabled = false
	if err := state.Atomic(path, disabled); err != nil {
		t.Fatal(err)
	}
	tick(inside.Add(time.Hour))
	if searches != 0 || fetches != 0 {
		t.Fatalf("unexpected execution outside the active idle window: %d searches, %d fetches", searches, fetches)
	}
	if err := state.Atomic(path, cfg); err != nil {
		t.Fatal(err)
	}
	tick(inside.Add(time.Hour))
	if searches != 1 || fetches != 2 {
		t.Fatalf("approved run executed %d searches and %d fetches", searches, fetches)
	}
	run, err := c.ResearchStatus(ctx, cfg, id, allow)
	if err != nil || run.State != learning.RunCompleted || run.Spent.Requests != 5 {
		t.Fatalf("scheduled run result = %+v, %v", run, err)
	}
	secondID, err := c.EnqueueResearch(ctx, cfg, "A second research question", 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ScheduleResearch(ctx, cfg, secondID, allow); err != nil {
		t.Fatal(err)
	}
	tick(inside.Add(90 * time.Minute))
	if searches != 1 || fetches != 2 || provider.requests != nil {
		t.Fatal("a second run or an unsolicited inference was triggered in the same window")
	}
	if err := r.Handle(ctx, input(2, 1, 0, 1, "Tell me about historical regency appointments")); err != nil {
		t.Fatal(err)
	}
	r.Wait()
	if len(provider.requests) != 1 || !slices.ContainsFunc(provider.requests[0].Messages, func(m llm.Message) bool {
		return m.Origin == priorResearchOrigin && m.Untrusted && strings.Contains(m.Content, first) &&
			strings.Contains(m.Content, second) && strings.Contains(m.Content, "bytes")
	}) {
		t.Fatalf("scheduled note was not recalled with both citations: %+v", provider.requests)
	}
}
