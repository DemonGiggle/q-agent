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
	if err := runLearning(ctx, configure, strings.NewReader(`{"enabled":true,"source_policy":{"domains":{"*.example.org":{"single_authority":true}}}}`), out); err == nil {
		t.Fatal("configure accepted wildcard authority")
	}
	policy := `{"enabled":true,"permissions":{"excluded_terms":["blocked"],"allow_domains":["example.org"]},"source_policy":{"max_age_days":30,"domains":{"records.example.org":{"single_authority":true,"max_age_days":90}}}}`
	if err := runLearning(ctx, configure, strings.NewReader(policy), out); err != nil {
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
	requirements := preview.Plan.EvidenceRequirements
	if !requirements.AllowSingleAuthority || requirements.MinIndependentSources != 2 ||
		requirements.SourcePolicy == nil || requirements.SourcePolicy.MaxAgeDays != 30 {
		t.Fatalf("unexpected preview source policy: %+v", requirements)
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
		got := learningMemoryLimits(app, startupLearningConfig(app))
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

func TestLearningStatusReadsMetadataWithoutLease(t *testing.T) {
	dir := t.TempDir()
	app := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(app, []byte("state_dir = 'data'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	out := new(bytes.Buffer)
	run := func(command, data string) error {
		out.Reset()
		return runLearning(ctx, []string{command, "--config", app}, strings.NewReader(data), out)
	}
	if err := run("configure", `{"enabled":true}`); err != nil {
		t.Fatal(err)
	}
	const question = "A distinct phrase to avoid leaking in status"
	if err := run("enqueue", question); err != nil {
		t.Fatal(err)
	}
	var original state.LearningWorkerResult
	if err := json.Unmarshal(out.Bytes(), &original); err != nil || original.RunID == "" {
		t.Fatalf("enqueue = %s, %v", out.String(), err)
	}
	unlock, err := state.Lock(filepath.Join(dir, "data", "learning"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := run("status", original.RunID); err != nil {
		t.Fatal(err)
	}
	var status state.LearningWorkerResult
	if err := json.Unmarshal(out.Bytes(), &status); err != nil || status.State != learning.RunQueued ||
		len(status.Transitions) != 1 || status.Transitions[0].EventID == "" || strings.Contains(out.String(), question) {
		t.Fatalf("status = %s, %v", out.String(), err)
	}
}

func TestLearningPauseAndResumeQueuedRun(t *testing.T) {
	dir := t.TempDir()
	app := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(app, []byte("state_dir = 'data'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	out := new(bytes.Buffer)
	command := func(action, input string) error {
		out.Reset()
		return runLearning(ctx, []string{action, "--config", app}, strings.NewReader(input), out)
	}
	if err := command("configure", `{"enabled":true}`); err != nil {
		t.Fatal(err)
	}
	if err := command("enqueue", "A public research question"); err != nil {
		t.Fatal(err)
	}
	var result state.LearningWorkerResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.RunID == "" {
		t.Fatalf("enqueue = %s, %v", out.String(), err)
	}
	id := result.RunID
	assert := func(action string, want learning.RunState, transitions int) {
		t.Helper()
		if err := command(action, id); err != nil {
			t.Fatal(err)
		}
		result = state.LearningWorkerResult{}
		if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.State != want ||
			len(result.Transitions) != transitions || result.Spent.Requests != 0 {
			t.Fatalf("%s = %s, %v", action, out.String(), err)
		}
	}
	assert("pause", learning.RunPaused, 2)
	assert("pause", learning.RunPaused, 2)
	assert("work", learning.RunPaused, 2)
	assert("resume", learning.RunQueued, 3)
	assert("resume", learning.RunQueued, 3)
}

func TestLearningAuditReplaysAfterStorageFailure(t *testing.T) {
	dir := t.TempDir()
	app := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(app, []byte("state_dir = 'data'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var out bytes.Buffer
	command := func(action, input string) error {
		out.Reset()
		return runLearning(ctx, []string{action, "--config", app}, strings.NewReader(input), &out)
	}
	if err := command("configure", `{"enabled":true}`); err != nil {
		t.Fatal(err)
	}
	auditDir := filepath.Join(dir, "data", "learning", "audit")
	if err := os.WriteFile(auditDir, nil, 0600); err != nil {
		t.Fatal(err)
	}
	const question = "Distinct text for audit privacy checks"
	if err := command("enqueue", question); err != nil {
		t.Fatal(err)
	}
	var status state.LearningWorkerResult
	if err := json.Unmarshal(out.Bytes(), &status); err != nil || status.RunID == "" {
		t.Fatalf("enqueue = %s, %v", &out, err)
	}
	id := status.RunID
	if err := command("pause", id); err != nil {
		t.Fatal(err)
	}
	if err := command("audit", id); err == nil {
		t.Fatal("audit succeeded with unavailable storage")
	}
	if err := os.Remove(auditDir); err != nil {
		t.Fatal(err)
	}
	if err := command("resume", id); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out.Bytes(), &status); err != nil || len(status.Transitions) != 3 {
		t.Fatalf("resume = %s, %v", &out, err)
	}
	entries, err := os.ReadDir(auditDir)
	if err != nil || len(entries) != 3 {
		t.Fatalf("automatic replay saved %d files, %v", len(entries), err)
	}
	if err := command("audit", id); err != nil {
		t.Fatal(err)
	}
	var events []learning.Event
	if err := json.Unmarshal(out.Bytes(), &events); err != nil || len(events) != 3 || strings.Contains(out.String(), question) {
		t.Fatalf("audit = %s, %v", &out, err)
	}
	for i, event := range events {
		transition := status.Transitions[i]
		if event.Type != learning.ResearchTransition || event.RunID != id || event.ID != transition.EventID ||
			event.Outcome != string(transition.State) || event.Reason != transition.Reason || !event.At.Equal(transition.At) ||
			event.Spent == nil || *event.Spent != *transition.Usage || len(event.ObjectRefs) != 1 {
			t.Fatalf("event %d = %+v, transition = %+v", i, event, transition)
		}
	}
	first := out.String()
	if err := os.Remove(filepath.Join(auditDir, events[0].ID+".json")); err != nil {
		t.Fatal(err)
	}
	type replay struct {
		output string
		err    error
	}
	results := make(chan replay, 2)
	for range cap(results) {
		go func() {
			var buf bytes.Buffer
			err := runLearning(ctx, []string{"audit", "--config", app}, strings.NewReader(id), &buf)
			results <- replay{buf.String(), err}
		}()
	}
	for range cap(results) {
		got := <-results
		if got.err != nil || got.output != first {
			t.Errorf("audit replay = %s, %v", got.output, got.err)
		}
	}
	entries, err = os.ReadDir(auditDir)
	if err != nil || len(entries) != 3 {
		t.Fatalf("explicit replay saved %d files, %v", len(entries), err)
	}
	if err := command("configure", `{"enabled":true,"permissions":{"excluded_terms":["Distinct text"]}}`); err != nil {
		t.Fatal(err)
	}
	if err := command("audit", id); !errors.Is(err, learning.ErrDenied) || out.Len() != 0 {
		t.Fatalf("audit of excluded question = %s, %v", &out, err)
	}
}
