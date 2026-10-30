package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"q-agent/internal/platform"
	"q-agent/internal/sandbox"
	"q-agent/internal/state"
	"runtime"
	"time"
)

type Image struct {
	Suite        string `json:"suite"`
	Architecture string `json:"architecture"`
	SHA256       string `json:"sha256"`
	Size         int64  `json:"size"`
}

func digest(path string) (Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return Image{}, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return Image{Suite: "trixie", Architecture: runtime.GOARCH, SHA256: hex.EncodeToString(h.Sum(nil)), Size: n}, err
}
func CheckImage(path string) error {
	data, err := os.ReadFile(path + ".json")
	if err != nil {
		return fmt.Errorf("completed base image required; run q-agent image build: %w", err)
	}
	var want Image
	if err = json.Unmarshal(data, &want); err != nil {
		return err
	}
	actual, err := digest(path)
	if err != nil {
		return err
	}
	if want != actual {
		return fmt.Errorf("base image checksum, suite, or native architecture mismatch")
	}
	return nil
}
func BuildImage(ctx context.Context, path, mmdebstrap string, output io.Writer) error {
	if err := CheckImage(path); err == nil {
		return nil
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("image exists without valid metadata; move it aside before rebuilding")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".image-*.tar")
	if err != nil {
		return err
	}
	tmp := f.Name()
	f.Close()
	os.Remove(tmp)
	defer os.Remove(tmp)
	cmd := exec.CommandContext(ctx, mmdebstrap, "--mode=unshare", "--variant=apt", "--include=ca-certificates", "--format=tar", "trixie", tmp)
	cmd.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/nonexistent", "LANG=C.UTF-8"}
	cmd.Stdout = output
	cmd.Stderr = output
	if err = cmd.Run(); err != nil {
		return fmt.Errorf("rootless mmdebstrap: %w", err)
	}
	image, err := digest(tmp)
	if err != nil {
		return err
	}
	f, err = os.Open(tmp)
	if err != nil {
		return err
	}
	err = f.Sync()
	f.Close()
	if err != nil {
		return err
	}
	if err = os.Chmod(tmp, 0600); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	if err = state.SyncDir(filepath.Dir(path)); err != nil {
		return err
	}
	return state.Atomic(path+".json", image)
}

type Owner struct {
	Platform string `json:"platform"`
	ChatID   int64  `json:"chat_id"`
}
type Manager struct {
	Dir, Image string
	Sandbox    sandbox.Options
}

// validateInboxRoot ensures root is a published rootfs under this manager so
// a caller cannot trick the sandbox bind into writing outside the chat tree.
func (m *Manager) validateInboxRoot(root string) error {
	cleanRoot := filepath.Clean(root)
	cleanDir := filepath.Clean(m.Dir)
	if !filepath.IsAbs(cleanRoot) || !filepath.IsAbs(cleanDir) {
		return fmt.Errorf("invalid workspace root")
	}
	rel, err := filepath.Rel(cleanDir, cleanRoot)
	if err != nil || rel == "." || rel == ".." || filepath.IsAbs(rel) {
		return fmt.Errorf("invalid workspace root")
	}
	// Expected layout: <dir>/<workspace-key>/rootfs
	if filepath.Base(cleanRoot) != "rootfs" || filepath.Dir(rel) == "." {
		return fmt.Errorf("invalid workspace root")
	}
	st, err := os.Lstat(cleanRoot)
	if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("workspace rootfs missing or unsafe")
	}
	return nil
}

// WriteInboxFile persists an uploaded attachment via the sandbox namespace,
// where the inbox directory is writable. The host cannot MkdirAll/WriteFile
// directly because rootfs/workspace is owned by the subordinate UID after
// extraction (0755, no host write). It returns the final basename after
// dedup suffixing.
func (m *Manager) WriteInboxFile(ctx context.Context, root, name string, data []byte, maxBytes int64) (string, error) {
	if err := m.validateInboxRoot(root); err != nil {
		return "", err
	}
	return sandbox.PutInboxFile(ctx, m.Sandbox, root, name, data, maxBytes)
}

// ClearInbox removes inbox entries older than cutoff via the sandbox
// namespace, where directory write permission exists for subordinate-UID
// trees. Missing inboxes are a no-op (0,0,nil).
func (m *Manager) ClearInbox(ctx context.Context, root string, cutoff time.Time) (int, int64, error) {
	if err := m.validateInboxRoot(root); err != nil {
		return 0, 0, err
	}
	inbox := filepath.Join(root, "workspace", "inbox")
	if st, err := os.Lstat(inbox); err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, err
	} else if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return 0, 0, fmt.Errorf("inbox is not a directory")
	}
	return sandbox.ClearInbox(ctx, m.Sandbox, root, cutoff)
}

// Ensure publishes the owner and rootfs together, only after extraction succeeds.
// The caller holds the workspace execution slot throughout this operation.
func (m *Manager) Peek(msg platform.Message) (string, bool) {
	owner := Owner{msg.Platform, msg.ChatID}
	base := filepath.Join(m.Dir, msg.WorkspaceKey())
	root := filepath.Join(base, "rootfs")
	data, err := os.ReadFile(filepath.Join(base, "owner.json"))
	if err != nil {
		return "", false
	}
	var saved Owner
	if json.Unmarshal(data, &saved) != nil || saved != owner {
		return "", false
	}
	if st, e := os.Lstat(root); e != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return "", false
	}
	return root, true
}

// Ensure publishes the owner and rootfs together, only after extraction succeeds.
// The caller holds the workspace execution slot throughout this operation.
func (m *Manager) Ensure(ctx context.Context, msg platform.Message) (string, error) {
	owner := Owner{msg.Platform, msg.ChatID}
	base := filepath.Join(m.Dir, msg.WorkspaceKey())
	root := filepath.Join(base, "rootfs")
	if data, err := os.ReadFile(filepath.Join(base, "owner.json")); err == nil {
		var saved Owner
		if json.Unmarshal(data, &saved) != nil || saved != owner {
			return "", fmt.Errorf("workspace ownership mismatch")
		}
		if st, e := os.Lstat(root); e != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("workspace rootfs missing or unsafe")
		}
		return root, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if _, err := os.Lstat(base); err == nil {
		return "", fmt.Errorf("incomplete published workspace; operator repair required")
	}
	if err := os.MkdirAll(m.Dir, 0700); err != nil {
		return "", err
	}
	staging, err := os.MkdirTemp(m.Dir, ".provision-")
	if err != nil {
		return "", err
	}
	success := false
	defer func() {
		if !success {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = sandbox.Remove(cleanupCtx, m.Sandbox, staging)
			_ = os.RemoveAll(staging)
		}
	}()
	stagedRoot := filepath.Join(staging, "rootfs")
	if err = os.Mkdir(stagedRoot, 0700); err != nil {
		return "", err
	}
	if err = sandbox.Provision(ctx, m.Sandbox, m.Image, stagedRoot); err != nil {
		return "", err
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err = state.Atomic(filepath.Join(staging, "owner.json"), owner); err != nil {
		return "", err
	}
	if err = state.SyncDir(staging); err != nil {
		return "", err
	}
	if err = os.Rename(staging, base); err != nil {
		return "", err
	}
	if err = state.SyncDir(m.Dir); err != nil {
		return "", err
	}
	success = true
	return root, nil
}
