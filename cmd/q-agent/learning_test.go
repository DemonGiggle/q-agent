package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"q-agent/internal/config"
	"q-agent/internal/learning"
	"q-agent/internal/platform"
	"q-agent/internal/state"
)

func TestLearningConfigureAndDryRun(t *testing.T) {
	dir := t.TempDir()
	app := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(app, []byte("state_dir = 'data'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	out := new(bytes.Buffer)
	configure := []string{"configure", "--config", app}
	trigger := []string{"trigger", "--config", app}
	if err := runLearning(ctx, configure, strings.NewReader(`{"enabled":true,"schedule":{"enabled":true}}`), out); err == nil {
		t.Fatal("configure accepted enabled schedule")
	}
	if err := runLearning(ctx, configure, strings.NewReader(`{"enabled":true,"permissions":{"excluded_terms":["blocked"]}}`), out); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runLearning(ctx, trigger, strings.NewReader("blocked question"), out); err == nil {
		t.Fatal("dry run accepted excluded question")
	}
	question := "Where can I read about the Fujiwara regency?"
	if err := runLearning(ctx, trigger, strings.NewReader(question), out); err != nil {
		t.Fatal(err)
	}
	var preview learning.Preview
	if err := json.Unmarshal(out.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if preview.Plan.QuestionID != preview.Question.ID || preview.Run.PlanID != preview.Plan.ID {
		t.Fatalf("broken preview references: %+v", preview)
	}
	dir = filepath.Join(dir, "data")
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "learning" {
		t.Fatalf("dry-run touched q-agent state: %v, %v", entries, err)
	}
	audit := filepath.Join(dir, "learning", "audit")
	events, err := os.ReadDir(audit)
	if err != nil || len(events) != 1 {
		t.Fatalf("audit files: %v, %v", events, err)
	}
	raw, err := os.ReadFile(filepath.Join(audit, events[0].Name()))
	if err != nil || bytes.Contains(raw, []byte(question)) {
		t.Fatalf("audit contents invalid: err=%v", err)
	}
	var event learning.Event
	if err := json.Unmarshal(raw, &event); err != nil {
		t.Fatal(err)
	}
	if err := event.Validate(); err != nil || event.RunID != preview.Run.ID {
		t.Fatalf("invalid audit event: %+v, %v", event, err)
	}
}

func TestLearningCapturePolicy(t *testing.T) {
	app := config.Defaults()
	app.StateDir = t.TempDir()
	check := func(want bool) {
		t.Helper()
		got := learningMemoryLimits(app)
		if got.LearnFromDMs != want || got.LearnFromGroups {
			t.Fatalf("unexpected capture policy: %+v", got)
		}
	}
	check(false)
	path := learningPolicyPath(app.StateDir)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"enabled":true,"permissions":{"include_private_chats":true}}`), 0600); err != nil {
		t.Fatal(err)
	}
	check(true)
	app.Memory.Enabled = false
	check(false)
}

func TestLearningSearchDeniesEgressBeforeCreatingRun(t *testing.T) {
	dir := t.TempDir()
	app := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(app, []byte("state_dir = 'data'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	configure := []string{"configure", "--config", app}
	search := []string{"search", "--config", app}
	out := new(bytes.Buffer)
	for _, tc := range []struct{ policy, query string }{
		{`{"enabled":false}`, "some question"},
		{`{"enabled":true,"permissions":{"block_domains":["duckduckgo.com"]}}`, "some question"},
		{`{"enabled":true}`, "api_key=sk-abcdefghijklmnopqrstuvwxyz"},
	} {
		if err := runLearning(ctx, configure, strings.NewReader(tc.policy), out); err != nil {
			t.Fatal(err)
		}
		out.Reset()
		if err := runLearning(ctx, search, strings.NewReader(tc.query), out); err != learning.ErrDenied || out.Len() != 0 {
			t.Fatalf("search returned %v with %d output bytes", err, out.Len())
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "data", "learning", "runs")); !os.IsNotExist(err) {
		t.Fatalf("denied query created run state: %v", err)
	}
}

func TestLearningFrontierUsesOnlyAuthorizedTopic(t *testing.T) {
	dir := t.TempDir()
	app := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(app, []byte("state_dir = 'data'\n[telegram]\nallowed_groups = [-100]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(filepath.Join(dir, "data"), nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(-time.Minute)
	for i, question := range []string{"藤原氏如何建立攝關政治？", "另一個話題有何不同？"} {
		route := platform.Message{Platform: "telegram", ChatID: -100, TopicID: int64(i + 7)}
		source := []string{"update:telegram:1", "update:telegram:2"}[i]
		if err := store.CommitExchange(route, source, state.Conversation{Pairs: []state.Pair{{User: question}}}, now); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	out := new(bytes.Buffer)
	configure := func(policy string) {
		t.Helper()
		if err := runLearning(ctx, []string{"configure", "--config", app}, strings.NewReader(policy), out); err != nil {
			t.Fatal(err)
		}
		out.Reset()
	}
	args := []string{"frontier", "--chat-id", "-100", "--topic-id", "7", "--config", app}
	configure(`{"enabled":true}`)
	if err := runLearning(ctx, args, strings.NewReader(""), out); !errors.Is(err, learning.ErrDenied) {
		t.Fatalf("missing group opt-in: %v", err)
	}
	configure(`{"enabled":true,"permissions":{"include_group_chats":true}}`)
	for range 2 {
		if err := runLearning(ctx, args, strings.NewReader(""), out); err != nil {
			t.Fatal(err)
		}
		var entries []learning.FrontierEntry
		if err := json.Unmarshal(out.Bytes(), &entries); err != nil || len(entries) != 1 ||
			len(entries[0].Question.OriginRefs) != 1 || !strings.Contains(entries[0].Question.Text, "藤原氏") {
			t.Fatalf("frontier = %s, %v", out.String(), err)
		}
		out.Reset()
	}
	selectArgs := append(append([]string(nil), args...), "--select")
	if err := runLearning(ctx, selectArgs, strings.NewReader(""), out); err != nil {
		t.Fatal(err)
	}
	var selected []learning.SelectedCandidate
	if err := json.Unmarshal(out.Bytes(), &selected); err != nil || len(selected) != 1 || selected[0].Reason != "origin_quota" {
		t.Fatalf("shortlist = %s, %v", out.String(), err)
	}
	out.Reset()
	configure(`{"enabled":true,"permissions":{"include_group_chats":true},"selection":{"conversation_percent":0,"gap_percent":100,"association_percent":0}}`)
	if err := runLearning(ctx, selectArgs, strings.NewReader(""), out); err != nil || strings.TrimSpace(out.String()) != "[]" {
		t.Fatalf("updated selection = %s, %v", out.String(), err)
	}
	out.Reset()

	configure(`{"enabled":true,"permissions":{"include_group_chats":true,"excluded_terms":["藤原氏"]}}`)
	if err := runLearning(ctx, args, strings.NewReader(""), out); err != nil || strings.TrimSpace(out.String()) != "[]" {
		t.Fatalf("filtered frontier = %s, %v", out.String(), err)
	}
}
