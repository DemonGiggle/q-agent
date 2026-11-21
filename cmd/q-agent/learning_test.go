package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"q-agent/internal/learning"
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
