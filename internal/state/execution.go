package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const ExecutionHost = "host"
const ExecutionSandbox = "sandbox"

// SetExecutionMode must finish before polling, delivery, or agent work starts.
// Missing metadata means legacy sandbox state. A failed or interrupted reset
// leaves the old mode on disk so the next launch repeats the entire reset.
func (s *Store) SetExecutionMode(mode string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if mode != ExecutionHost && mode != ExecutionSandbox {
		return fmt.Errorf("invalid execution mode")
	}
	if err := s.recoverHistory(); err != nil {
		return err
	}
	path := filepath.Join(s.dir, "execution.json")
	saved := struct {
		Mode string `json:"mode"`
	}{Mode: ExecutionSandbox}
	data, err := os.ReadFile(path)
	if err == nil {
		saved.Mode = ""
		if json.Unmarshal(data, &saved) != nil || saved.Mode != ExecutionHost && saved.Mode != ExecutionSandbox {
			return fmt.Errorf("invalid execution mode metadata")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if saved.Mode != mode {
		dir := filepath.Join(s.dir, "conversations")
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			if !entry.Type().IsRegular() {
				return fmt.Errorf("unsafe conversation state during execution mode reset")
			}
			if err = Atomic(filepath.Join(dir, entry.Name()), Conversation{}); err != nil {
				return fmt.Errorf("reset conversation for execution mode change: %w", err)
			}
		}
	}
	return Atomic(path, struct {
		Mode string `json:"mode"`
	}{Mode: mode})
}
