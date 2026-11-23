package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"q-agent/internal/learning"
	"q-agent/internal/state"
)

func TestLearningDigestUsesAuthorizedRecentNotes(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	app := filepath.Join(dir, "config.toml")
	writeCfg := func(enabled bool) {
		t.Helper()
		body := "state_dir = 'data'\n[learning]\nenabled = false\n"
		if enabled {
			body = "state_dir = 'data'\n[learning]\nenabled = true\nallow_domains = ['example.org', 'example.net']\nblock_domains = ['blocked.example.net']\nexcluded_terms = ['restricted phrase']\n"
		}
		if err := os.WriteFile(app, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeCfg(true)
	stateDir := filepath.Join(dir, "data")
	now := time.Now().UTC()
	snapshot := learning.KnowledgeSnapshot{Version: 1}
	for _, item := range []struct {
		title, domain string
		age           time.Duration
		archived      bool
	}{
		{"New public note", "example.org", time.Hour, false},
		{"Second public note", "example.org", 2 * time.Hour, false},
		{"Hidden domain note", "blocked.example.net", 10 * time.Minute, false},
		{"Archived note", "example.org", 15 * time.Minute, true},
		{"Note with a restricted phrase", "example.org", 20 * time.Minute, false},
		{"Future note", "example.org", -time.Hour, false},
		{"Old note recently edited", "example.org", 48 * time.Hour, false},
	} {
		created := now.Add(-item.age)
		id := rand.Text()
		source := learning.Source{ID: id, CanonicalURI: "https://" + item.domain + "/archive", FetchedAt: now.Add(-7 * 24 * time.Hour)}
		note := learning.Note{Record: learning.Record{CreatedAt: created, UpdatedAt: now}, ID: rand.Text(),
			Title: item.title, Summary: "Public material from the archive.", SourceIDs: []string{id}}
		if item.archived {
			note.ArchivedAt = &now
		}
		snapshot.Sources = append(snapshot.Sources, source)
		snapshot.Notes = append(snapshot.Notes, note)
	}
	path := filepath.Join(stateDir, "learning", "knowledge", "public.json")
	if err := state.Atomic(path, struct {
		Schema   string                     `json:"schema_version"`
		Snapshot learning.KnowledgeSnapshot `json:"snapshot"`
	}{learning.SchemaVersion, snapshot}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := runLearning(ctx, []string{"digest", "--config", app, "--limit", "1"}, nil, &out); err != nil {
		t.Fatal(err)
	}
	if text := out.String(); strings.Count(text, "## Research question") != 1 || !strings.Contains(text, "New public note") ||
		!strings.Contains(text, "https://example.org/archive") || strings.Contains(text, "Second public note") {
		t.Fatalf("unexpected digest: %s", text)
	}
	out.Reset()
	if err := runLearning(ctx, []string{"digest", "--config", app}, nil, &out); err != nil {
		t.Fatal(err)
	}
	if text := out.String(); strings.Count(text, "## Research question") != 2 || !strings.Contains(text, "Second public note") {
		t.Fatalf("digest included stale, future, archived or excluded notes: %s", text)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("digest rewrote knowledge: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "learning", "audit")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("digest unexpectedly recorded an audit event: %v", err)
	}
	writeCfg(false)
	if err := runLearning(ctx, []string{"digest", "--config", app}, nil, &out); !errors.Is(err, learning.ErrDenied) {
		t.Fatalf("digest was available when learning was disabled: %v", err)
	}
}
