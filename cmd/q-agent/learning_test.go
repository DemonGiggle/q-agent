package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
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
	if err := runLearning(ctx, configure, strings.NewReader(`{"schedule":{"enabled":true}}`), out); err == nil {
		t.Fatal("configure accepted a schedule when learning is disabled")
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
	if err != nil || len(events) != 2 {
		t.Fatalf("audit files: %v, %v", events, err)
	}
	for _, item := range events {
		raw, err := os.ReadFile(filepath.Join(audit, item.Name()))
		if err != nil || bytes.Contains(raw, []byte(question)) || bytes.Contains(raw, []byte("example.org")) {
			t.Fatalf("audit contents invalid: err=%v", err)
		}
		var event learning.Event
		if err := json.Unmarshal(raw, &event); err != nil {
			t.Fatal(err)
		}
		if err := event.Validate(); err != nil || (event.Type == learning.ResearchPlanned && event.RunID != preview.Run.ID) {
			t.Fatalf("invalid audit event: %+v, %v", event, err)
		}
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
	configure(`{"enabled":true,"permissions":{"include_group_chats":true}}`)
	queue := append(append([]string(nil), args...), "--enqueue", selected[0].Question.ID)
	if err := runLearning(ctx, queue, strings.NewReader(""), out); !errors.Is(err, learning.ErrDenied) {
		t.Fatalf("chat candidate was queued without a public question: %v", err)
	}
	const approved = "Which archives document regency appointments?"
	var id string
	for range 2 {
		if err := runLearning(ctx, queue, strings.NewReader(approved), out); err != nil {
			t.Fatal(err)
		}
		var handoff state.FrontierHandoff
		if err := json.Unmarshal(out.Bytes(), &handoff); err != nil || handoff.RunID == "" ||
			(id != "" && handoff.RunID != id) {
			t.Fatalf("handoff = %s, %v", out.String(), err)
		}
		id = handoff.RunID
		out.Reset()
	}
	raw, err := os.ReadFile(filepath.Join(dir, "data", "learning", "runs", id+".json"))
	if err != nil || !bytes.Contains(raw, []byte(approved)) || bytes.Contains(raw, []byte("藤原氏")) ||
		bytes.Contains(raw, []byte("update:telegram")) {
		t.Fatalf("unexpected public run = %s, %v", raw, err)
	}
	if err := runLearning(ctx, queue, strings.NewReader("Different question"), out); !errors.Is(err, learning.ErrVersionConflict) {
		t.Fatalf("replaced a previously approved public query: %v", err)
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
	if err := os.Rename(auditDir, auditDir+".before"); err != nil {
		t.Fatal(err)
	}
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
	if err := os.Rename(auditDir+".before", auditDir); err != nil {
		t.Fatal(err)
	}
	if err := command("resume", id); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out.Bytes(), &status); err != nil || len(status.Transitions) != 3 {
		t.Fatalf("resume = %s, %v", &out, err)
	}
	entries, err := os.ReadDir(auditDir)
	if err != nil || len(entries) != 4 {
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
	if err != nil || len(entries) != 4 {
		t.Fatalf("explicit replay saved %d files, %v", len(entries), err)
	}
	if err := command("configure", `{"enabled":true,"permissions":{"excluded_terms":["Distinct text"]}}`); err != nil {
		t.Fatal(err)
	}
	if err := command("audit", id); !errors.Is(err, learning.ErrDenied) || out.Len() != 0 {
		t.Fatalf("audit of excluded question = %s, %v", &out, err)
	}
}

func TestLearningPrivateFrontierLifecycle(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	app := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(app, []byte("state_dir = 'data'\n[telegram]\nallowed_groups = [-100]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(filepath.Join(dir, "data"), nil)
	if err != nil {
		t.Fatal(err)
	}
	const erasedText = "What made the private astronomical archive unusual?"
	const retainedText = "What methods did the paper workshop use?"
	for i, message := range []struct {
		topic int64
		text  string
	}{
		{7, erasedText}, {7, retainedText}, {8, "How was the water mill built?"},
	} {
		route := platform.Message{Platform: "telegram", ChatID: -100, TopicID: message.topic}
		if err := store.CommitExchange(route, fmt.Sprintf("update:telegram:%d", i+1), state.Conversation{Pairs: []state.Pair{{User: message.text}}}, time.Now().Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	invoke := func(input string, args ...string) (string, error) {
		var out strings.Builder
		err := runLearning(ctx, append(args, "--config", app), strings.NewReader(input), &out)
		return strings.TrimSpace(out.String()), err
	}
	run := func(input string, args ...string) string {
		t.Helper()
		out, err := invoke(input, args...)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	const enabled = `{"enabled":true,"permissions":{"include_group_chats":true,"allow_domains":["example.org"]}}`
	run(enabled, "configure")
	var entries []learning.FrontierEntry
	if err := json.Unmarshal([]byte(run("", "frontier", "--chat-id", "-100", "--topic-id", "7")), &entries); err != nil || len(entries) != 2 {
		t.Fatalf("frontier = %+v, %v", entries, err)
	}
	var eraseID, retainedID string
	for _, entry := range entries {
		if strings.Contains(entry.Question.Text, "astronomical archive") {
			eraseID = entry.Question.ID
		} else {
			retainedID = entry.Question.ID
		}
	}
	if eraseID == "" || retainedID == "" {
		t.Fatalf("lost a private lead: %+v", entries)
	}
	run("", "frontier", "--chat-id", "-100", "--topic-id", "7", "--mute", retainedID)
	if out := run("", "frontier", "--chat-id", "-100", "--topic-id", "7", "--select"); strings.Contains(out, retainedID) {
		t.Fatalf("muted candidate selected: %s", out)
	}
	run("", "frontier", "--chat-id", "-100", "--topic-id", "7", "--pin", retainedID)
	if out := run("", "frontier", "--chat-id", "-100", "--topic-id", "7", "--select"); !strings.Contains(out, "manual_priority") {
		t.Fatalf("pinned candidate omitted: %s", out)
	}
	dir = filepath.Join(dir, "data", "learning", "knowledge")
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("partition files = %v, %v", files, err)
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, copy := range []string{files[0] + ".bak.1", filepath.Join(dir, ".atomic-123")} {
		if err := os.WriteFile(copy, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	run(`{"enabled":true,"permissions":{"include_group_chats":true,"excluded_topics":[{"chat_id":-100,"topic_id":7}]}}`, "configure")
	if _, err := invoke("", "export", "--chat-id", "-100", "--topic-id", "7"); !errors.Is(err, learning.ErrDenied) {
		t.Fatalf("blacklisted topic exported: %v", err)
	}
	run(`{"enabled":false}`, "configure")
	id := rand.Text()
	args := []string{"delete", "--chat-id", "-100", "--topic-id", "7", "--candidate", eraseID, "--request-id", id}
	first := run("", args...)
	if second := run("", args...); first != second || strings.Contains(first, "astronomical") {
		t.Fatalf("receipt is unstable or contains deleted text: %s, %s", first, second)
	}
	if _, err := invoke("", "delete", "--chat-id", "-99", "--topic-id", "7"); !errors.Is(err, learning.ErrDenied) {
		t.Fatalf("unauthorized scope erased: %v", err)
	}
	raw, err = os.ReadFile(files[0])
	if err != nil || bytes.Contains(raw, []byte("astronomical")) || !bytes.Contains(raw, []byte("paper workshop")) {
		t.Fatalf("wrong data after erasure: %s, %v", raw, err)
	}
	for _, copy := range []string{files[0] + ".bak.1", filepath.Join(dir, ".atomic-123")} {
		if _, err := os.Stat(copy); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("erased copy %s is still present: %v", copy, err)
		}
	}
	run(enabled, "configure")
	if out := run("", "frontier", "--chat-id", "-100", "--topic-id", "7"); strings.Contains(out, "astronomical") || !strings.Contains(out, "paper workshop") {
		t.Fatalf("frontier after reopening: %s", out)
	}
	if out := run("", "export", "--public"); strings.Contains(out, "paper workshop") || strings.Contains(out, "astronomical") {
		t.Fatalf("private data leaked into public export: %s", out)
	}
	if out := run("", "frontier", "--chat-id", "-100", "--topic-id", "8"); !strings.Contains(out, "water mill") {
		t.Fatalf("another topic affected by deletion: %s", out)
	}
	run("", "frontier", "--chat-id", "-100", "--topic-id", "7", "--dismiss", retainedID)
	if out := run("", "frontier", "--chat-id", "-100", "--topic-id", "7"); out != "[]" {
		t.Fatalf("dismissed candidate shown as active: %s", out)
	}
	run("", "delete", "--chat-id", "-100", "--topic-id", "7")
	if out := run("", "frontier", "--chat-id", "-100", "--topic-id", "7", "--status", "all"); out != "[]" {
		t.Fatalf("erased topic unexpectedly rebuilt: %s", out)
	}
}
