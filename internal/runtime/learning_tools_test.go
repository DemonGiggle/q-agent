package runtime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"q-agent/internal/learning"
	"q-agent/internal/llm"
	"q-agent/internal/state"
)

type learningToolProvider struct {
	mu       sync.Mutex
	requests []llm.Request
	replies  []llm.Message
}

func (p *learningToolProvider) Name() string { return "ollama" }

func (p *learningToolProvider) Complete(_ context.Context, req llm.Request, _ llm.StreamCallback) (llm.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	idx := len(p.requests)
	p.requests = append(p.requests, req)
	if idx < len(p.replies) {
		return llm.Response{Message: p.replies[idx]}, nil
	}
	return llm.Response{Message: llm.Message{Content: "done"}}, nil
}

func writeLearningFixture(t *testing.T, stateDir string, notes []learning.Note, sources []learning.Source, evidence []learning.Evidence, claims []learning.Claim) {
	t.Helper()
	snapshot := learning.KnowledgeSnapshot{
		Version:  1,
		Notes:    notes,
		Sources:  sources,
		Evidence: evidence,
		Claims:   claims,
	}
	path := filepath.Join(stateDir, "learning", "knowledge", "public.json")
	if err := state.Atomic(path, struct {
		Schema   string                     `json:"schema_version"`
		Snapshot learning.KnowledgeSnapshot `json:"snapshot"`
	}{learning.SchemaVersion, snapshot}); err != nil {
		t.Fatal(err)
	}
}

func makeFixtureNote(t *testing.T, now time.Time, title, summary, uri, quote, subject, pred, obj string) (learning.Note, learning.Source, learning.Evidence, learning.Claim) {
	t.Helper()
	rec := learning.Record{SchemaVersion: learning.SchemaVersion, CreatedAt: now, UpdatedAt: now}
	src := learning.Source{
		Record:       rec,
		ID:           rand.Text(),
		CanonicalURI: uri,
		ContentHash:  fmt.Sprintf("%x", sha256.Sum256([]byte(quote))),
		FetchedAt:    now,
	}
	ev, err := learning.SelectSourceExcerpt(learning.FetchedDocument{Source: src, Body: []byte(quote)}, []learning.DocumentSpan{{
		Locator: learning.Locator{Selector: learning.SourceBytesSelector, Start: 0, End: len(quote)},
		Text:    quote,
	}}, quote, func(string) bool { return true })
	if err != nil || ev == nil {
		t.Fatalf("evidence fixture = %+v, %v", ev, err)
	}
	claim := learning.Claim{
		Record:    rec,
		ID:        rand.Text(),
		Subject:   subject,
		Predicate: pred,
		Object:    obj,
		Revision:  1,
		RevisedAt: now,
		Status:    "hypothesis",
		Evidence:  []learning.EvidenceLink{{EvidenceID: ev.ID, Relation: "supports"}},
	}
	note := learning.Note{
		Record:      rec,
		ID:          rand.Text(),
		Title:       title,
		Summary:     summary,
		SourceIDs:   []string{src.ID},
		EvidenceIDs: []string{ev.ID},
		ClaimIDs:    []string{claim.ID},
		Revision:    1,
	}
	return note, src, *ev, claim
}

func hasTool(tools []llm.Tool, name string) bool {
	return slices.ContainsFunc(tools, func(t llm.Tool) bool { return t.Name == name })
}

func TestLearningRecallToolLastRunAndLockout(t *testing.T) {
	r, _, _ := fixture(t)
	r.Learning = learning.DefaultConfig()
	r.Learning.Enabled = true
	r.Learning.Recall = learning.RecallConfig{ChatEnabled: true, AuditEnabled: true}
	r.Learning.Permissions.AllowDomains = []string{"example.org"}

	now := time.Now().UTC().Truncate(time.Second)
	olderTime := now.Add(-72 * time.Hour)
	newerTime := now.Add(-6 * time.Hour)
	oldNote, oldSrc, oldEv, oldClaim := makeFixtureNote(t, olderTime, "Older Topic", "Older summary.", "https://docs.example.org/old", "Older verified excerpt text.", "Old", "is", "archived")
	newNote, newSrc, newEv, newClaim := makeFixtureNote(t, newerTime, "Kyber Lattice Notes", "Overnight summary of ML-KEM.", "https://docs.example.org/kyber", "ML-KEM uses module learning with errors.", "ML-KEM", "uses", "MLWE")
	writeLearningFixture(t, r.Config.StateDir,
		[]learning.Note{oldNote, newNote},
		[]learning.Source{oldSrc, newSrc},
		[]learning.Evidence{oldEv, newEv},
		[]learning.Claim{oldClaim, newClaim},
	)

	provider := &learningToolProvider{
		replies: []llm.Message{
			{ToolCalls: []llm.ToolCall{{ID: "c1", Name: learningRecallName, Arguments: json.RawMessage(`{}`)}}},
			{Content: "Last night I learned about ML-KEM."},
		},
	}
	r.Providers = func(state.Conversation) (llm.Provider, error) { return provider, nil }

	ctx := context.Background()
	if err := r.Handle(ctx, input(1, 1, 0, 1, "what have you learnt last time?")); err != nil {
		t.Fatal(err)
	}
	r.Wait()

	if len(provider.requests) != 2 {
		t.Fatalf("expected 2 model steps, got %d", len(provider.requests))
	}
	if !hasTool(provider.requests[0].Tools, learningRecallName) {
		t.Fatalf("step 1 missing %s tool: %+v", learningRecallName, provider.requests[0].Tools)
	}
	// Post-recall tool lockout: step 2 must advertise zero tools.
	if len(provider.requests[1].Tools) != 0 {
		t.Fatalf("expected zero tools after recall, got %d: %+v", len(provider.requests[1].Tools), provider.requests[1].Tools)
	}
	// Verify tool result contains newest note first and cites URL/excerpt.
	var toolMsg llm.Message
	for _, m := range provider.requests[1].Messages {
		if m.Role == "tool" && m.Name == learningRecallName {
			toolMsg = m
		}
	}
	if toolMsg.Content == "" || !toolMsg.Untrusted {
		t.Fatalf("missing untrusted tool message: %+v", toolMsg)
	}
	if !strings.Contains(toolMsg.Content, "Kyber Lattice Notes") || !strings.Contains(toolMsg.Content, "https://docs.example.org/kyber") {
		t.Fatalf("tool output missing newest note: %s", toolMsg.Content)
	}
	if strings.Index(toolMsg.Content, "Kyber Lattice Notes") > strings.Index(toolMsg.Content, "Older Topic") {
		t.Fatalf("expected newest note before older note: %s", toolMsg.Content)
	}

	// Verify RecallUsed audit event was written with IDs only.
	auditDir := filepath.Join(r.Config.StateDir, "learning", "audit")
	files, err := os.ReadDir(auditDir)
	if err != nil || len(files) != 1 {
		t.Fatalf("audit files = %v, %v", files, err)
	}
	raw, err := os.ReadFile(filepath.Join(auditDir, files[0].Name()))
	var ev learning.Event
	if err != nil || json.Unmarshal(raw, &ev) != nil || ev.Type != learning.RecallUsed ||
		!slices.Contains(ev.ObjectRefs, newNote.ID) || !slices.Contains(ev.ClaimRefs, newClaim.ID) ||
		strings.Contains(string(raw), "Kyber") || strings.Contains(string(raw), "https://docs.example.org/kyber") {
		t.Fatalf("unexpected recall audit event: %s, %v", raw, err)
	}
}

func TestLearningRecallToolDaysQueryAndValidation(t *testing.T) {
	r, _, _ := fixture(t)
	r.Learning = learning.DefaultConfig()
	r.Learning.Enabled = true
	r.Learning.Recall = learning.RecallConfig{ChatEnabled: true}
	r.Learning.Permissions.AllowDomains = []string{"example.org"}

	now := time.Now().UTC().Truncate(time.Second)
	oldNote, oldSrc, oldEv, oldClaim := makeFixtureNote(t, now.Add(-5*24*time.Hour), "Dilithium Notes", "Signatures summary.", "https://crypto.example.org/dilithium", "Dilithium is a lattice signature scheme.", "Dilithium", "is", "signature")
	newNote, newSrc, newEv, newClaim := makeFixtureNote(t, now.Add(-2*time.Hour), "Kyber Notes", "KEM summary.", "https://crypto.example.org/kyber", "Kyber is a lattice key encapsulation mechanism.", "Kyber", "is", "KEM")
	writeLearningFixture(t, r.Config.StateDir,
		[]learning.Note{oldNote, newNote},
		[]learning.Source{oldSrc, newSrc},
		[]learning.Evidence{oldEv, newEv},
		[]learning.Claim{oldClaim, newClaim},
	)

	owner := input(1, 1, 0, 1, "test")
	ctx := context.Background()

	// days=1 returns only the recent note, not the 5-day-old one.
	lt := &learningTools{base: noTools{}, runtime: r, owner: owner}
	out, err := lt.Execute(ctx, llm.ToolCall{Name: learningRecallName, Arguments: json.RawMessage(`{"days":1,"limit":5}`)})
	if err != nil {
		t.Fatalf("days=1 failed: %v", err)
	}
	if !strings.Contains(out, "Kyber Notes") || strings.Contains(out, "Dilithium Notes") {
		t.Fatalf("days=1 output = %s", out)
	}
	// After recall succeeds, any further tool call is denied.
	if _, err := lt.Execute(ctx, llm.ToolCall{Name: "shell", Arguments: json.RawMessage(`{"command":"ls"}`)}); !errors.Is(err, learning.ErrDenied) {
		t.Fatalf("expected ErrDenied after recall lockout, got %v", err)
	}

	// query searches by keyword across all ages.
	lt2 := &learningTools{base: noTools{}, runtime: r, owner: owner}
	out, err = lt2.Execute(ctx, llm.ToolCall{Name: learningRecallName, Arguments: json.RawMessage(`{"query":"Dilithium"}`)})
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if !strings.Contains(out, "Dilithium Notes") || strings.Contains(out, "Kyber Notes") {
		t.Fatalf("query output = %s", out)
	}

	// Empty result does NOT lock out tools.
	lt3 := &learningTools{base: noTools{}, runtime: r, owner: owner}
	out, err = lt3.Execute(ctx, llm.ToolCall{Name: learningRecallName, Arguments: json.RawMessage(`{"query":"nonexistent_term_xyz"}`)})
	if err != nil {
		t.Fatalf("empty query failed: %v", err)
	}
	if !strings.Contains(out, "No authorized learning notes found") {
		t.Fatalf("unexpected empty output: %s", out)
	}
	if !hasTool(lt3.Schemas(), learningRecallName) {
		t.Fatal("empty recall should not lock out tools")
	}

	// Invalid argument combinations are rejected.
	lt4 := &learningTools{base: noTools{}, runtime: r, owner: owner}
	for _, rawArgs := range []string{
		`{"days":1,"query":"Kyber"}`,
		`{"days":31}`,
		`{"limit":6}`,
	} {
		if _, err := lt4.Execute(ctx, llm.ToolCall{Name: learningRecallName, Arguments: json.RawMessage(rawArgs)}); err == nil {
			t.Fatalf("expected error for args %s", rawArgs)
		}
	}
}

func TestLearningRecallToolPolicyGating(t *testing.T) {
	r, _, _ := fixture(t)
	r.Learning = learning.DefaultConfig()
	ctx := context.Background()

	// Disabled by default -> not advertised and execution denied.
	owner := input(1, 1, 0, 1, "hello")
	lt := &learningTools{base: noTools{}, runtime: r, owner: owner}
	if hasTool(lt.Schemas(), learningRecallName) {
		t.Fatal("tool advertised while learning is disabled")
	}
	if _, err := lt.Execute(ctx, llm.ToolCall{Name: learningRecallName, Arguments: json.RawMessage(`{}`)}); !errors.Is(err, learning.ErrDenied) {
		t.Fatalf("expected ErrDenied when disabled, got %v", err)
	}

	// Enabled without chat_recall -> still not advertised.
	r.Learning.Enabled = true
	if hasTool(lt.Schemas(), learningRecallName) {
		t.Fatal("tool advertised without chat_recall")
	}

	// Enabled with chat_recall -> advertised in DM, not in group without IncludeGroupChats.
	r.Learning.Recall.ChatEnabled = true
	if !hasTool(lt.Schemas(), learningRecallName) {
		t.Fatal("tool not advertised in authorized DM")
	}
	groupTool := &learningTools{base: noTools{}, runtime: r, owner: input(2, -10, 0, 1, "hello")}
	if hasTool(groupTool.Schemas(), learningRecallName) {
		t.Fatal("tool advertised in group without IncludeGroupChats")
	}

	// Excluded topic -> not advertised.
	r.Learning.Permissions.ExcludedTopics = []learning.ConversationTopic{{ChatID: 1, TopicID: 0}}
	if hasTool(lt.Schemas(), learningRecallName) {
		t.Fatal("tool advertised in excluded topic")
	}
}
