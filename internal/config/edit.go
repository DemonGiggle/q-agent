package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"

	"github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/unstable"
)

// Values returns the effective configuration using the documented TOML keys.
// It contains credential references, never resolved credential values.
// Deprecated approved_* keys are hidden; use allowed_users/allowed_groups.
func Values(c Config) map[string]any {
	result := map[string]any{}
	var walk func(reflect.Value, string)
	walk = func(v reflect.Value, prefix string) {
		for i := 0; i < v.NumField(); i++ {
			key := prefix + v.Type().Field(i).Tag.Get("toml")
			if v.Field(i).Kind() == reflect.Map {
				iter := v.Field(i).MapRange()
				for iter.Next() {
					walk(iter.Value(), key+"."+iter.Key().String()+".")
				}
			} else if v.Field(i).Kind() == reflect.Struct {
				walk(v.Field(i), key+".")
			} else {
				result[key] = v.Field(i).Interface()
			}
		}
	}
	walk(reflect.ValueOf(c), "")
	delete(result, "telegram.approved_user_ids")
	delete(result, "telegram.approved_chat_ids")
	return result
}

// legacyKeyHint reports the replacement for removed allowlist keys.
func legacyKeyHint(key string) bool {
	return key == "telegram.approved_user_ids" || key == "telegram.approved_chat_ids"
}

// ParseValue accepts plain strings and TOML literals for numeric/bool/array fields.
func ParseValue(key, text string) (any, error) {
	if legacyKeyHint(key) {
		return nil, fmt.Errorf("%q was replaced by telegram.allowed_users and telegram.allowed_groups; old configs migrate automatically on load", key)
	}
	exemplar, ok := Values(Defaults())[key]
	if !ok {
		return nil, fmt.Errorf("unknown configuration key %q", key)
	}
	if _, ok := exemplar.(string); ok {
		return text, nil
	}
	holder := reflect.New(reflect.TypeOf(exemplar))
	// Decode with a typed holder to reject fractional integers and wrong types.
	typ := reflect.StructOf([]reflect.StructField{{Name: "Value", Type: holder.Elem().Type(), Tag: `toml:"value"`}})
	target := reflect.New(typ)
	if err := toml.NewDecoder(strings.NewReader("value = " + text)).DisallowUnknownFields().Decode(target.Interface()); err != nil {
		return nil, fmt.Errorf("invalid value for %s (expected %s)", key, holder.Elem().Type())
	}
	return target.Elem().Field(0).Interface(), nil
}

type textEdit struct {
	start, end int
	value      string
}

func nodeKey(n *unstable.Node) string {
	var keys []string
	it := n.Key()
	for it.Next() {
		keys = append(keys, string(it.Node().Data))
	}
	return strings.Join(keys, ".")
}

// patch changes only selected expressions. Unrelated text (including comments)
// remains byte-for-byte intact. Unusual unsupported layouts fail validation
// before the real file is replaced.
func patch(data []byte, changes map[string]any) ([]byte, error) {
	remaining := map[string]string{}
	for key, value := range changes {
		if legacyKeyHint(key) {
			return nil, fmt.Errorf("%q was replaced by telegram.allowed_users and telegram.allowed_groups; old configs migrate automatically on load", key)
		}
		sample, ok := Values(Defaults())[key]
		if !ok || reflect.TypeOf(value) != reflect.TypeOf(sample) {
			return nil, fmt.Errorf("invalid configuration key/type %q", key)
		}
		encoded, err := toml.Marshal(map[string]any{"value": value})
		if err != nil {
			return nil, err
		}
		remaining[key] = strings.TrimSpace(strings.SplitN(string(encoded), "=", 2)[1])
	}
	var edits []textEdit
	sections := map[string]int{"": 0}
	section := ""
	var parser unstable.Parser
	parser.Reset(data)
	for parser.NextExpression() {
		n := parser.Expression()
		switch n.Kind {
		case unstable.Table:
			section = nodeKey(n)
			// Table.Raw is not populated by go-toml's parser. Key ranges are.
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
			key := nodeKey(n)
			if section != "" {
				key = section + "." + key
			}
			if value, ok := remaining[key]; ok {
				start, end := int(n.Raw.Offset), int(n.Raw.Offset+n.Raw.Length)
				// KeyValue.Raw includes the entire value, including multiline arrays.
				// The parsed key can itself contain '=' inside quotes, so use its end.
				it := n.Key()
				keyEnd := start
				for it.Next() {
					k := it.Node()
					keyEnd = int(k.Raw.Offset + k.Raw.Length)
				}
				eq := bytes.IndexByte(data[keyEnd:end], '=')
				if eq < 0 {
					return nil, fmt.Errorf("cannot edit %s", key)
				}
				edits = append(edits, textEdit{start, end, string(data[start:keyEnd+eq+1]) + " " + value})
				delete(remaining, key)
			}
		}
	}
	if err := parser.Error(); err != nil {
		return nil, fmt.Errorf("cannot parse configuration")
	}
	// Aggregate insertions at the same position to keep ordering deterministic.
	inserts := map[int]string{}
	keys := make([]string, 0, len(remaining))
	for key := range remaining {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	newSections := map[string]string{}
	for _, key := range keys {
		table, field := "", key
		if i := strings.LastIndex(key, "."); i >= 0 {
			table, field = key[:i], key[i+1:]
		}
		line := field + " = " + remaining[key] + "\n"
		if pos, ok := sections[table]; ok {
			inserts[pos] += line
		} else {
			newSections[table] += line
		}
	}
	for pos, value := range inserts {
		if pos > 0 && data[pos-1] != '\n' {
			value = "\n" + value
		}
		edits = append(edits, textEdit{pos, pos, value})
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
	tables := make([]string, 0, len(newSections))
	for table := range newSections {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	for _, table := range tables {
		result = append(result, []byte("\n["+table+"]\n"+newSections[table])...)
	}
	return result, nil
}

// Update serializes CLI writers, validates the candidate in the original
// directory (so relative paths keep their meaning), then atomically replaces it.
// No credential files are read and no running service is touched.
func Update(path string, changes map[string]any) error {
	path, err := absolute(path, ".")
	if err != nil {
		return err
	}
	lockFD, err := syscall.Open(path+".lock", syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return fmt.Errorf("open config lock: %w", err)
	}
	lock := os.NewFile(uintptr(lockFD), path+".lock")
	defer lock.Close()
	if err = syscall.Flock(lockFD, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("configuration is being edited by another process")
	}
	defer syscall.Flock(lockFD, syscall.LOCK_UN)
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("configuration must be a regular file, not a symlink")
	}
	if info.Mode().Perm()&0200 == 0 {
		return fmt.Errorf("configuration must be writable by its owner")
	}
	if _, err = Load(path); err != nil {
		return fmt.Errorf("existing configuration is invalid")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	candidate, err := patch(data, changes)
	if err != nil {
		return err
	}
	if bytes.Equal(data, candidate) {
		return nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".q-agent-config-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if _, err = tmp.Write(candidate); err != nil {
		return err
	}
	if _, err = Load(tmp.Name()); err != nil {
		return fmt.Errorf("updated configuration is invalid; no changes saved")
	}
	if err = tmp.Chmod(info.Mode().Perm()); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	current, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	live, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !os.SameFile(info, live) || !bytes.Equal(current, data) {
		return fmt.Errorf("configuration changed during edit; no changes saved")
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
