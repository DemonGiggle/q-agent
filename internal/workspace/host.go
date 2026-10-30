package workspace

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"q-agent/internal/platform"
	"q-agent/internal/state"
)

type HostManager struct{ Dir string }

// Peek returns the workspace root without provisioning. It reports false when
// the workspace was never published so daily sweeps can skip it cheaply.
func (m *HostManager) Peek(msg platform.Message) (string, bool) {
	owner := Owner{msg.Platform, msg.ChatID}
	base := filepath.Join(m.Dir, msg.WorkspaceKey())
	root := filepath.Join(base, "workspace")
	st, err := os.Lstat(base)
	if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return "", false
	}
	data, err := os.ReadFile(filepath.Join(base, "owner.json"))
	if err != nil {
		return "", false
	}
	var saved Owner
	if json.Unmarshal(data, &saved) != nil || saved != owner {
		return "", false
	}
	if st, err = os.Lstat(root); err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return "", false
	}
	return root, true
}

// Ensure atomically publishes a private chat folder with permanent ownership.
// Topics use the same WorkspaceKey. The runtime holds the chat execution slot.
func (m *HostManager) Ensure(ctx context.Context, msg platform.Message) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	owner := Owner{msg.Platform, msg.ChatID}
	base := filepath.Join(m.Dir, msg.WorkspaceKey())
	root := filepath.Join(base, "workspace")
	if st, err := os.Lstat(base); err == nil {
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("host workspace missing or unsafe")
		}
		data, err := os.ReadFile(filepath.Join(base, "owner.json"))
		if err != nil {
			return "", fmt.Errorf("incomplete published host workspace: %w", err)
		}
		var saved Owner
		if json.Unmarshal(data, &saved) != nil || saved != owner {
			return "", fmt.Errorf("workspace ownership mismatch")
		}
		if st, err = os.Lstat(root); err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("host workspace directory missing or unsafe")
		}
		return root, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := os.MkdirAll(m.Dir, 0700); err != nil {
		return "", err
	}
	staging, err := os.MkdirTemp(m.Dir, ".provision-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(staging)
	stagedRoot := filepath.Join(staging, "workspace")
	if err = os.Mkdir(stagedRoot, 0700); err != nil {
		return "", err
	}
	if err = state.SyncDir(stagedRoot); err != nil {
		return "", err
	}
	if err = state.Atomic(filepath.Join(staging, "owner.json"), owner); err != nil {
		return "", err
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	if err = os.Rename(staging, base); err != nil {
		return "", err
	}
	if err = state.SyncDir(m.Dir); err != nil {
		return "", err
	}
	return root, nil
}
