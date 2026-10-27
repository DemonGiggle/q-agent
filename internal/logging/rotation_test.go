package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestDailyRotationIncludesExistingLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "serve.log")
	now := time.Now()
	if err := os.WriteFile(path, []byte("legacy\n"), 0600); err != nil {
		t.Fatal(err)
	}
	old := now.AddDate(0, 0, -1)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	w := &Writer{Path: path, Now: func() time.Time { return now }}
	if _, err := w.Write([]byte("today\n")); err != nil {
		t.Fatal(err)
	}
	if read(t, path) != "today\n" || read(t, path+".1") != "legacy\n" {
		t.Fatal("existing log was not retained")
	}
	if _, err := w.Write([]byte("more\n")); err != nil {
		t.Fatal(err)
	}
	if read(t, path) != "today\nmore\n" {
		t.Fatal("rotated more than once on the same day")
	}
	for i := 1; i <= 7; i++ {
		previous := now.AddDate(0, 0, -1)
		if err := os.Chtimes(path, previous, previous); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(fmt.Sprintf("day %d\n", i))); err != nil {
			t.Fatal(err)
		}
	}
	if read(t, path) != "day 7\n" || read(t, path+".6") != "day 1\n" {
		t.Fatal("wrong retention ordering")
	}
	if _, err := os.Stat(path + ".7"); !os.IsNotExist(err) {
		t.Fatal("retained more than seven files")
	}
}

func TestMigrateLegacyHelperLogs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "helper.log")
	oldA := filepath.Join(dir, "helper-111111111111111111111111.log")
	oldB := filepath.Join(dir, "helper-222222222222222222222222.log")
	for name, text := range map[string]string{path: "current\n", path + ".1": "previous\n", oldA: "first\n", oldB: "second\n", filepath.Join(dir, "helper-unknown.log"): "unrelated"} {
		if err := os.WriteFile(name, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	w := &Writer{Path: path}
	if err := w.MigrateLegacyHelpers(); err != nil {
		t.Fatal(err)
	}
	if read(t, path) != "current\n" || read(t, path+".1") != "first\nsecond\n" || read(t, path+".2") != "previous\n" {
		t.Fatal("migration lost existing logs")
	}
	if _, err := os.Stat(oldA); !os.IsNotExist(err) {
		t.Fatal("legacy file not removed")
	}
	if read(t, filepath.Join(dir, "helper-unknown.log")) != "unrelated" {
		t.Fatal("changed unrelated file")
	}
	if err := w.MigrateLegacyHelpers(); err != nil {
		t.Fatal(err)
	}
	if read(t, path+".1") != "first\nsecond\n" {
		t.Fatal("migration was not idempotent")
	}
}

func TestConcurrentWritersAndSupervisor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "serve.log")
	var group sync.WaitGroup
	for i := 0; i < 4; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			w := &Writer{Path: path}
			for j := 0; j < 25; j++ {
				if _, err := w.Write([]byte("line\n")); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	group.Wait()
	if got := strings.Count(read(t, path), "line\n"); got != 100 {
		t.Fatalf("lost writes: %d", got)
	}
	if err := Run(path, []string{"/bin/sh", "-c", "printf stdout; printf stderr >&2"}); err != nil {
		t.Fatal(err)
	}
	content := read(t, path)
	if !strings.Contains(content, "stdout") || !strings.Contains(content, "stderr") {
		t.Fatalf("missing process output: %q", content)
	}
}
