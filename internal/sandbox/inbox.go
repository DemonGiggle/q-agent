package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// InboxDirMode allows the host service user to list/traverse an inbox
// created inside the user namespace. The parent workspaces tree stays 0700
// host-side, so 0755 inside remains private to the service account (plus
// namespace root). Host 0700 would hide the inbox from the host sweeper;
// 0755 is required because sandbox-owned files live under a subordinate UID.
const InboxDirMode = 0755

// InboxFileMode allows the host to read inbox files for quota accounting
// and photo uploads. Sandbox tools run as namespace root and can read
// anything; the host has no capabilities, so 0600 sandbox-owned files would
// be unreadable (and undeletable without directory write) from the host.
const InboxFileMode = 0644

func validateInboxName(name string) error {
	if name == "" || name == "." || name == ".." {
		return fmt.Errorf("invalid inbox name")
	}
	if len(name) > 100 {
		return fmt.Errorf("invalid inbox name")
	}
	if strings.Contains(name, "/") || strings.Contains(name, "\\") || strings.Contains(name, "\x00") {
		return fmt.Errorf("invalid inbox name")
	}
	if filepath.Base(name) != name {
		return fmt.Errorf("invalid inbox name")
	}
	cleaned := filepath.Clean(name)
	if cleaned != name || cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return fmt.Errorf("invalid inbox name")
	}
	return nil
}

func ensureInboxDir(dir string) error {
	st, err := os.Lstat(dir)
	if err == nil {
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("inbox is not a directory")
		}
		// Normalize to 0755 so host listing keeps working after the ownership
		// flips to the subordinate UID. Best effort: a chmod failure still
		// leaves a usable directory when we are not namespace root.
		_ = os.Chmod(dir, InboxDirMode)
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(dir, InboxDirMode); err != nil {
		return err
	}
	// Re-validate after creation to close symlink-plant races on parents.
	if err := parentsSafe(filepath.Dir(dir), filepath.Base(dir)); err != nil {
		return err
	}
	st, err = os.Lstat(dir)
	if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("inbox is not a directory")
	}
	_ = os.Chmod(dir, InboxDirMode)
	return nil
}

func inboxUsageLocked(dir string) int64 {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	var total int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if info, err := e.Info(); err == nil {
			total += info.Size()
		}
	}
	return total
}

func uniqueInboxBase(dir, name string) string {
	base := filepath.Base(name)
	if base == "" || base == "." || base == "/" {
		base = "file.bin"
	}
	candidate := base
	if _, err := os.Lstat(filepath.Join(dir, candidate)); os.IsNotExist(err) {
		return candidate
	}
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	for i := 1; i < 1000; i++ {
		candidate = fmt.Sprintf("%s-%d%s", stem, i, ext)
		if _, err := os.Lstat(filepath.Join(dir, candidate)); os.IsNotExist(err) {
			return candidate
		}
	}
	return candidate
}

// writeInboxData validates, enforces quota, and persists data atomically with
// O_EXCL so concurrent saves and running agent tools cannot clobber each
// other. It returns the final basename (after dedup suffixing).
func writeInboxData(dir, name string, data []byte, maxBytes int64) (string, error) {
	if err := validateInboxName(name); err != nil {
		return "", err
	}
	if err := ensureInboxDir(dir); err != nil {
		return "", err
	}
	if maxBytes <= 0 {
		return "", fmt.Errorf("invalid inbox quota")
	}
	if int64(len(data))+inboxUsageLocked(dir) > maxBytes {
		return "", fmt.Errorf("chat inbox is full (100 MB limit); remove files from inbox/ and resend")
	}
	// Resolve a non-existent candidate first, then create exclusively so a
	// concurrent writer racing the same name gets EEXIST and retries.
	for i := 0; i < 1000; i++ {
		candidate := name
		if i > 0 {
			ext := filepath.Ext(name)
			stem := strings.TrimSuffix(name, ext)
			candidate = fmt.Sprintf("%s-%d%s", stem, i, ext)
		}
		dst := filepath.Join(dir, candidate)
		// Containment: Join + Rel must stay inside dir even under races.
		if rel, err := filepath.Rel(dir, dst); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == "." {
			return "", fmt.Errorf("invalid inbox name")
		}
		f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			if os.IsExist(err) {
				continue
			}
			return "", err
		}
		_, werr := f.Write(data)
		serr := f.Sync()
		cerr := f.Close()
		if werr != nil {
			_ = os.Remove(dst)
			return "", werr
		}
		if serr != nil {
			_ = os.Remove(dst)
			return "", serr
		}
		if cerr != nil {
			_ = os.Remove(dst)
			return "", cerr
		}
		// 0644 so the host (no caps, other UID) can read for sendPhoto and
		// quota; parents stay 0700 host-side so this remains account-private.
		_ = os.Chmod(dst, InboxFileMode)
		if err := syncDir(dir); err != nil {
			return "", err
		}
		return candidate, nil
	}
	return "", fmt.Errorf("could not allocate inbox name")
}

func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// clearInboxEntries removes entries older than cutoff without following
// symlinks and never removes dir itself. Missing directories are a no-op.
func clearInboxEntries(dir string, cutoff time.Time) (int, int64) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0
	}
	var removed int
	var freed int64
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		info, err := os.Lstat(p)
		if err != nil {
			continue
		}
		if !info.ModTime().Before(cutoff) {
			continue
		}
		if info.IsDir() {
			if err := os.RemoveAll(p); err == nil {
				removed++
			}
			continue
		}
		size := info.Size()
		if err := os.Remove(p); err == nil {
			removed++
			freed += size
		}
	}
	if removed > 0 {
		_ = syncDir(dir)
	}
	return removed, freed
}
