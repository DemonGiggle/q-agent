package state

import (
	"fmt"
	"path/filepath"
)

// CompanionPref stores a workspace-scoped companion override shared by all
// topics in a chat, like ChatModel. Mode is one of default|on|off; empty
// means default (follow the global [proactive] enabled flag).
type CompanionPref struct {
	Mode string `json:"mode,omitempty"`
}

// GetCompanion loads the per-chat companion override. Missing files report
// the default (follow global).
func (s *Store) GetCompanion(key string) (CompanionPref, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var pref CompanionPref
	if err := read(filepath.Join(s.dir, "companion", key+".json"), &pref); err != nil {
		return pref, err
	}
	if pref.Mode == "" {
		pref.Mode = "default"
	}
	return pref, nil
}

// SetCompanion persists the per-chat companion override.
func (s *Store) SetCompanion(key string, pref CompanionPref) error {
	if pref.Mode == "" {
		pref.Mode = "default"
	}
	switch pref.Mode {
	case "default", "on", "off":
	default:
		return fmt.Errorf("invalid companion mode %q: want default|on|off", pref.Mode)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return Atomic(filepath.Join(s.dir, "companion", key+".json"), pref)
}

// CompanionEffective reports whether the companion sweep may visit a chat:
// per-chat on forces on even when the global flag is off, per-chat off
// forces off, otherwise the global [proactive] enabled flag decides.
func CompanionEffective(globalEnabled bool, pref CompanionPref) bool {
	switch pref.Mode {
	case "on":
		return true
	case "off":
		return false
	case "default", "":
		return globalEnabled
	default:
		return false
	}
}
