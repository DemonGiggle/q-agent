package sandbox

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestValidateInboxName(t *testing.T) {
	long := string(make([]byte, 0))
	for len(long) < 101 {
		long += "a"
	}
	long += ".txt" // >100 bytes total
	for _, bad := range []string{"", ".", "..", "../evil", "a/b", `a\b`, "/abs", "a\x00b", long} {
		if err := validateInboxName(bad); err == nil {
			t.Fatalf("accepted bad name %q", bad)
		}
	}
	for _, good := range []string{"logo.png", "photo-1-1.jpg", "notes.txt", "a b (1).txt", "file.bin"} {
		if err := validateInboxName(good); err != nil {
			t.Fatalf("rejected good name %q: %v", good, err)
		}
	}
}

func TestWriteInboxDataQuotaDedupAndPerms(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "inbox")
	if saved, err := writeInboxData(dir, "notes.txt", []byte("hello"), 100<<20); err != nil || saved != "notes.txt" {
		t.Fatalf("write=%q err=%v", saved, err)
	}
	// Dedup suffixing must be atomic-safe (O_EXCL loop).
	if saved, err := writeInboxData(dir, "notes.txt", []byte("world"), 100<<20); err != nil || saved != "notes-1.txt" {
		t.Fatalf("dedup=%q err=%v", saved, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "notes.txt"))
	if err != nil || string(data) != "hello" {
		t.Fatalf("saved=%q err=%v", string(data), err)
	}
	// Directory must be host-listable (0755) and files host-readable (0644)
	// so the host sweeper and sendPhoto path keep working for
	// subordinate-UID-owned trees.
	if st, err := os.Stat(dir); err != nil || st.Mode().Perm() != 0755 {
		t.Fatalf("inbox mode=%v err=%v", st, err)
	}
	if st, err := os.Stat(filepath.Join(dir, "notes.txt")); err != nil || st.Mode().Perm() != 0644 {
		t.Fatalf("file mode=%v err=%v", st, err)
	}
	// Quota enforced inside the writer so host pre-check races cannot overflow.
	if _, err := writeInboxData(dir, "big.bin", make([]byte, 100<<20), 100<<20); err == nil {
		t.Fatal("quota not enforced")
	}
	// Traversal rejected even after sanitization.
	if _, err := writeInboxData(dir, "../evil.sh", []byte("x"), 100<<20); err == nil {
		t.Fatal("traversal accepted")
	}
}

func TestClearInboxEntriesAgeAndSymlink(t *testing.T) {
	dir := t.TempDir()
	oldFile := filepath.Join(dir, "old.txt")
	newFile := filepath.Join(dir, "new.txt")
	if err := os.WriteFile(oldFile, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newFile, []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(oldFile, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	// Symlink must never be followed; Lstat path removes the link itself when old.
	link := filepath.Join(dir, "link")
	if err := os.Symlink(oldFile, link); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(link, oldTime, oldTime); err != nil {
		// lchtimes may fail on some filesystems; link age check uses Lstat
		// on the link itself, fall back to removing by name if needed.
		t.Logf("chtimes link: %v", err)
	}
	cutoff := time.Now().Add(-24 * time.Hour)
	removed, _ := clearInboxEntries(dir, cutoff)
	if removed == 0 {
		t.Fatal("nothing removed")
	}
	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Fatal("old file survived")
	}
	if _, err := os.Stat(newFile); err != nil {
		t.Fatalf("new file wrongly removed: %v", err)
	}
	if _, err := os.Lstat(dir); err != nil {
		t.Fatalf("inbox dir itself removed: %v", err)
	}
	// Missing dir is a no-op.
	if removed, freed := clearInboxEntries(filepath.Join(t.TempDir(), "nope"), cutoff); removed != 0 || freed != 0 {
		t.Fatalf("missing dir not no-op: %d/%d", removed, freed)
	}
}
