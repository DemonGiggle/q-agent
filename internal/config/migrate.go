package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/unstable"
)

// Report describes a load-time configuration file migration.
type Report struct {
	// Migrated is true when the file was rewritten on disk.
	Migrated bool
	// Backup holds the pre-migration copy. Empty when nothing was rewritten.
	Backup string
	// Changes names the applied migrations, without values. Empty when
	// nothing was rewritten.
	Changes []string
}

// Legacy keys handled by load-time auto-migration.
const maxToolCallsKey = "limits.max_tool_calls"

var legacyAllowlistKeys = map[string]bool{
	"telegram.approved_user_ids":  true,
	"telegram.approved_chat_ids":  true,
}

// occurrenceIDs decodes the value of one matched key/value expression as an
// ID array. Our matched keys are unquoted, so the first '=' separates key
// from value; trailing comments remain valid TOML.
func occurrenceIDs(data []byte, o keyOccurrence) ([]int64, error) {
	raw := data[o.start:o.end]
	eq := bytes.IndexByte(raw, '=')
	if eq < 0 {
		return nil, fmt.Errorf("cannot parse %s", o.key)
	}
	var holder struct {
		Value []int64 `toml:"value"`
	}
	if err := toml.Unmarshal(append([]byte("value = "), raw[eq+1:]...), &holder); err != nil {
		return nil, err
	}
	return holder.Value, nil
}

type keyOccurrence struct {
	key        string // dotted full key, e.g. telegram.approved_user_ids
	start, end int    // KeyValue.Raw range covering key and value
}

// scanKeys parses data once, returning every key/value expression with its
// dotted key and raw range, plus each table's header-end offset (the insert
// position used for new keys, matching patch).
func scanKeys(data []byte) (occs []keyOccurrence, sections map[string]int, err error) {
	sections = map[string]int{"": 0}
	section := ""
	var parser unstable.Parser
	parser.Reset(data)
	for parser.NextExpression() {
		n := parser.Expression()
		switch n.Kind {
		case unstable.Table:
			section = nodeKey(n)
			end := 0
			it := n.Key()
			for it.Next() {
				k := it.Node()
				end = int(k.Raw.Offset + k.Raw.Length)
			}
			if next := bytes.IndexByte(data[end:], '\n'); next >= 0 {
				end += next + 1
			} else {
				end = len(data)
			}
			sections[section] = end
		case unstable.KeyValue:
			bare := nodeKey(n)
			key := bare
			// Dotted keys (telegram.approved_user_ids = ...) are
			// absolute paths; only bare keys inherit the section.
			if !strings.Contains(bare, ".") && section != "" {
				key = section + "." + bare
			}
			occs = append(occs, keyOccurrence{
				key:   key,
				start: int(n.Raw.Offset),
				end:   int(n.Raw.Offset + n.Raw.Length),
			})
		}
	}
	if err := parser.Error(); err != nil {
		return nil, nil, fmt.Errorf("cannot parse configuration")
	}
	return occs, sections, nil
}

// lineSpan expands an expression range to whole lines so deletions leave no
// stray indentation or detached trailing comments behind.
func lineSpan(data []byte, start, end int) (int, int) {
	s := 0
	if i := bytes.LastIndexByte(data[:start], '\n'); i >= 0 {
		s = i + 1
	}
	e := len(data)
	if i := bytes.IndexByte(data[end:], '\n'); i >= 0 {
		e = end + i + 1
	}
	return s, e
}

func encodeIDs(ids []int64) (string, error) {
	encoded, err := toml.Marshal(map[string]any{"value": ids})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(strings.SplitN(string(encoded), "=", 2)[1]), nil
}

// planMigration inspects raw file content and returns the migrated candidate
// plus human-readable change notes. It returns needWrite=false when the file
// needs no migration. Truly unknown keys are left untouched so strict Load
// still rejects them afterwards.
func planMigration(data []byte) (candidate []byte, changes []string, needWrite bool, err error) {
	occs, sections, err := scanKeys(data)
	if err != nil {
		return nil, nil, false, err
	}
	found := map[string][]keyOccurrence{}
	for _, o := range occs {
		if legacyAllowlistKeys[o.key] || o.key == maxToolCallsKey ||
			o.key == "telegram.allowed_users" || o.key == "telegram.allowed_groups" {
			found[o.key] = append(found[o.key], o)
		}
	}
	if len(found) == 0 {
		return nil, nil, false, nil
	}

	// Extract ID arrays from the raw expressions. Struct decoding misses
	// top-level dotted keys, so values come straight from the text. Any
	// unparseable value aborts migration and leaves the file for strict
	// Load to report.
	var legacyUsers, legacyChats, newUsers, newGroups []int64
	for _, o := range found["telegram.approved_user_ids"] {
		ids, err := occurrenceIDs(data, o)
		if err != nil {
			return nil, nil, false, nil
		}
		legacyUsers = append(legacyUsers, ids...)
	}
	for _, o := range found["telegram.approved_chat_ids"] {
		ids, err := occurrenceIDs(data, o)
		if err != nil {
			return nil, nil, false, nil
		}
		legacyChats = append(legacyChats, ids...)
	}
	for _, o := range found["telegram.allowed_users"] {
		ids, err := occurrenceIDs(data, o)
		if err != nil {
			return nil, nil, false, nil
		}
		newUsers = append(newUsers, ids...)
	}
	for _, o := range found["telegram.allowed_groups"] {
		ids, err := occurrenceIDs(data, o)
		if err != nil {
			return nil, nil, false, nil
		}
		newGroups = append(newGroups, ids...)
	}

	// Mixing old and new allowlist keys is ambiguous; refuse to guess and
	// ask the operator to resolve it, mirroring Load.
	hasLegacy := len(legacyUsers) > 0 || len(legacyChats) > 0
	hasNew := len(newUsers) > 0 || len(newGroups) > 0
	if hasLegacy && hasNew {
		return nil, nil, false, fmt.Errorf("telegram.approved_user_ids/approved_chat_ids were replaced by telegram.allowed_users and telegram.allowed_groups; remove the old keys")
	}

	var edits []textEdit
	seenDelete := map[textEdit]bool{}
	del := func(o keyOccurrence) {
		s, e := lineSpan(data, o.start, o.end)
		t := textEdit{s, e, ""}
		if !seenDelete[t] {
			seenDelete[t] = true
			edits = append(edits, t)
		}
	}

	if occs := found[maxToolCallsKey]; len(occs) > 0 {
		for _, o := range occs {
			del(o)
		}
		changes = append(changes, "removed limits.max_tool_calls")
	}

	// Deprecated allowlists migrate with the same semantics as Load:
	// positive approved_user_ids (plus orphan positive approved_chat_ids)
	// become allowed_users, negative approved_chat_ids become allowed_groups.
	if n := len(found["telegram.approved_user_ids"]) + len(found["telegram.approved_chat_ids"]); n > 0 {
		seen := map[int64]bool{}
		var users, groups []int64
		for _, id := range legacyUsers {
			if id <= 0 {
				continue
			}
			if !seen[id] {
				seen[id] = true
				users = append(users, id)
			}
		}
		for _, id := range legacyChats {
			if id > 0 {
				if !seen[id] {
					seen[id] = true
					users = append(users, id)
				}
			} else if id < 0 {
				groups = append(groups, id)
			}
		}
		for _, o := range found["telegram.approved_user_ids"] {
			del(o)
		}
		for _, o := range found["telegram.approved_chat_ids"] {
			del(o)
		}
		inserts := map[string]string{}
		if len(users) > 0 {
			encoded, err := encodeIDs(users)
			if err != nil {
				return nil, nil, false, err
			}
			inserts["allowed_users"] = "allowed_users = " + encoded + "\n"
			changes = append(changes, "renamed telegram.approved_user_ids -> telegram.allowed_users")
		} else if len(found["telegram.approved_user_ids"]) > 0 {
			changes = append(changes, "removed empty telegram.approved_user_ids")
		}
		if len(groups) > 0 {
			encoded, err := encodeIDs(groups)
			if err != nil {
				return nil, nil, false, err
			}
			inserts["allowed_groups"] = "allowed_groups = " + encoded + "\n"
			changes = append(changes, "renamed telegram.approved_chat_ids -> telegram.allowed_groups")
		} else if len(found["telegram.approved_chat_ids"]) > 0 {
			changes = append(changes, "removed telegram.approved_chat_ids")
		}
		if len(inserts) > 0 {
			fields := make([]string, 0, len(inserts))
			for f := range inserts {
				fields = append(fields, f)
			}
			sort.Strings(fields)
			if pos, ok := sections["telegram"]; ok {
				combined := ""
				for _, f := range fields {
					combined += inserts[f]
				}
				edits = append(edits, textEdit{pos, pos, combined})
			} else {
				combined := ""
				for _, f := range fields {
					combined += inserts[f]
				}
				edits = append(edits, textEdit{len(data), len(data), "\n[telegram]\n" + combined})
			}
		}
	}

	if len(edits) == 0 {
		return nil, nil, false, nil
	}
	sort.Slice(edits, func(i, j int) bool {
		if edits[i].start == edits[j].start {
			return edits[i].end > edits[j].end
		}
		return edits[i].start > edits[j].start
	})
	result := append([]byte(nil), data...)
	for _, e := range edits {
		result = append(append(append([]byte(nil), result[:e.start]...), []byte(e.value)...), result[e.end:]...)
	}
	return result, changes, true, nil
}

// backupCopy writes data to a timestamped sibling of path with the given
// mode, never overwriting an existing backup.
func backupCopy(path string, data []byte, mode os.FileMode) (string, error) {
	stamp := time.Now().Format("20060102-150405")
	base := path + ".bak." + stamp
	target := base
	for i := 1; ; i++ {
		f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err != nil {
			if os.IsExist(err) && i < 100 {
				target = fmt.Sprintf("%s-%d", base, i)
				continue
			}
			return "", err
		}
		if _, err := f.Write(data); err != nil {
			f.Close()
			return "", err
		}
		if err := f.Sync(); err != nil {
			f.Close()
			return "", err
		}
		if err := f.Close(); err != nil {
			return "", err
		}
		return target, nil
	}
}

// MigrateIfNeeded rewrites path in place when it contains keys handled by
// load-time auto-migration (deprecated telegram allowlists, removed
// limits.max_tool_calls). It is idempotent: files without legacy keys are
// left untouched and no backup is created. Comments, unrelated keys, and
// file permissions are preserved; a timestamped backup of the original is
// kept next to the file.
//
// Files that fail to parse, mix old and new allowlist keys, or contain
// truly unknown keys are left unchanged (the latter surface later via
// strict Load). Read-only files with only allowlist legacy keys are also
// left unchanged so Load can keep serving them from its in-memory
// migration; read-only files needing removals fail with guidance.
func MigrateIfNeeded(path string) (Report, error) {
	path, err := absolute(path, ".")
	if err != nil {
		return Report{}, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		// Missing file: leave it to Load to report.
		return Report{}, nil
	}
	if !info.Mode().IsRegular() {
		return Report{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Report{}, nil
	}
	if len(data) > 1<<20 {
		return Report{}, nil
	}
	candidate, changes, needWrite, err := planMigration(data)
	if err != nil {
		return Report{}, err
	}
	if !needWrite {
		return Report{}, nil
	}
	writable := info.Mode().Perm()&0200 != 0
	if !writable {
		needsRemoval := false
		for _, c := range changes {
			if strings.HasPrefix(c, "removed limits.max_tool_calls") {
				needsRemoval = true
			}
		}
		if !needsRemoval {
			// Load already migrates allowlists in memory; keep serving
			// read-only files without touching them.
			return Report{}, nil
		}
		return Report{}, fmt.Errorf("configuration requires migration (%s) but is not writable; make it writable or edit manually", strings.Join(changes, "; "))
	}

	lockFD, err := syscall.Open(path+".lock", syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return Report{}, fmt.Errorf("open config lock: %w", err)
	}
	lock := os.NewFile(uintptr(lockFD), path+".lock")
	defer lock.Close()
	if err = syscall.Flock(lockFD, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return Report{}, fmt.Errorf("configuration is being edited by another process")
	}
	defer syscall.Flock(lockFD, syscall.LOCK_UN)

	// Re-read under lock; another writer may have migrated already.
	info, err = os.Lstat(path)
	if err != nil {
		return Report{}, err
	}
	if !info.Mode().IsRegular() {
		return Report{}, fmt.Errorf("configuration must be a regular file, not a symlink")
	}
	data, err = os.ReadFile(path)
	if err != nil {
		return Report{}, err
	}
	candidate, changes, needWrite, err = planMigration(data)
	if err != nil {
		return Report{}, err
	}
	if !needWrite {
		return Report{}, nil
	}

	backup, err := backupCopy(path, data, info.Mode().Perm())
	if err != nil {
		return Report{}, fmt.Errorf("cannot back up configuration: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".q-agent-config-*")
	if err != nil {
		return Report{}, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if _, err = tmp.Write(candidate); err != nil {
		return Report{}, err
	}
	if _, err = Load(tmp.Name()); err != nil {
		return Report{}, fmt.Errorf("migrated configuration is invalid; no changes saved")
	}
	if err = tmp.Chmod(info.Mode().Perm()); err != nil {
		return Report{}, err
	}
	if err = tmp.Sync(); err != nil {
		return Report{}, err
	}
	if err = tmp.Close(); err != nil {
		return Report{}, err
	}
	current, err := os.ReadFile(path)
	if err != nil {
		return Report{}, err
	}
	live, err := os.Lstat(path)
	if err != nil {
		return Report{}, err
	}
	if !os.SameFile(info, live) || !bytes.Equal(current, data) {
		return Report{}, fmt.Errorf("configuration changed during edit; no changes saved")
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return Report{}, err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return Report{}, err
	}
	defer dir.Close()
	if err = dir.Sync(); err != nil {
		return Report{}, err
	}
	return Report{Migrated: true, Backup: backup, Changes: changes}, nil
}

// LoadMigrated auto-migrates path on disk when needed, then loads it. It is
// the load-time entry point for long-lived processes and console commands;
// pure Load stays side-effect free for tests and validators.
func LoadMigrated(path string) (Config, Report, error) {
	rep, err := MigrateIfNeeded(path)
	if err != nil {
		c := Defaults()
		return c, rep, err
	}
	c, err := Load(path)
	return c, rep, err
}
