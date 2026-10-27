// Package logging keeps the detached service's diagnostic logs bounded by day.
package logging

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// RetainedFiles includes the active log and its numbered backups.
const RetainedFiles = 7

// Writer opens the active file for each write so independent supervisors do
// not continue writing to an archive after another process rotates it.
type Writer struct {
	Path string
	Now  func() time.Time // nil uses the local clock
}

func (w *Writer) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Writer) locked(fn func() error) error {
	if !filepath.IsAbs(w.Path) {
		return fmt.Errorf("log path must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(w.Path), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(w.Path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}

func (w *Writer) Write(p []byte) (n int, err error) {
	err = w.locked(func() error {
		now := w.now()
		info, statErr := os.Stat(w.Path)
		if statErr != nil && !os.IsNotExist(statErr) {
			return statErr
		}
		if statErr == nil && info.Size() > 0 && day(info.ModTime(), now.Location()) < day(now, now.Location()) {
			if err := shift(w.Path); err != nil {
				return err
			}
			if err := os.Rename(w.Path, w.Path+".1"); err != nil {
				return err
			}
		}
		f, err := os.OpenFile(w.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		n, err = f.Write(p)
		closeErr := f.Close()
		if err != nil {
			return err
		}
		return closeErr
	})
	return n, err
}

// Prepare detects inaccessible logs before a maintenance request is armed.
func (w *Writer) Prepare() error {
	if err := w.MigrateLegacyHelpers(); err != nil {
		return err
	}
	return w.locked(func() error {
		f, err := os.OpenFile(w.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		return f.Close()
	})
}

func day(t time.Time, loc *time.Location) string { return t.In(loc).Format("2006-01-02") }

// shift makes room for one backup, retaining the active file plus six backups.
func shift(path string) error {
	for i := RetainedFiles - 2; i >= 1; i-- {
		from, to := fmt.Sprintf("%s.%d", path, i), fmt.Sprintf("%s.%d", path, i+1)
		if err := os.Rename(from, to); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// MigrateLegacyHelpers consolidates the old per-request files into one backup.
// The original files are removed only after the combined backup is in place.
func (w *Writer) MigrateLegacyHelpers() error {
	return w.locked(func() error {
		if filepath.Base(w.Path) != "helper.log" {
			return nil
		}
		matches, err := filepath.Glob(filepath.Join(filepath.Dir(w.Path), "helper-*.log"))
		if err != nil {
			return err
		}
		var legacy []string
		for _, path := range matches {
			id := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "helper-"), ".log")
			if len(id) == 24 && strings.Trim(id, "0123456789abcdef") == "" {
				legacy = append(legacy, path)
			}
		}
		if len(legacy) == 0 {
			return nil
		}
		sort.Strings(legacy)
		tmp, err := os.CreateTemp(filepath.Dir(w.Path), ".helper-log-migrate-*")
		if err != nil {
			return err
		}
		defer os.Remove(tmp.Name())
		for _, path := range legacy {
			f, openErr := os.Open(path)
			if openErr != nil {
				tmp.Close()
				return openErr
			}
			_, copyErr := io.Copy(tmp, f)
			f.Close()
			if copyErr != nil {
				tmp.Close()
				return copyErr
			}
		}
		if err = tmp.Sync(); err != nil {
			tmp.Close()
			return err
		}
		if err = tmp.Close(); err != nil {
			return err
		}
		if err = shift(w.Path); err != nil {
			return err
		}
		if err = os.Rename(tmp.Name(), w.Path+".1"); err != nil {
			return err
		}
		for _, path := range legacy {
			if err = os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		return nil
	})
}
