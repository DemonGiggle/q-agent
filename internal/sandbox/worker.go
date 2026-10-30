package sandbox

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"q-agent/internal/localtools"
	"strings"
	"time"
)

// Worker is an internal command. It refuses to execute unless the namespace
// and service-user mapping match the isolation contract.
func Worker(mode string, in io.Reader, out io.Writer) error {
	if err := becomeRoot(); err != nil {
		return err
	}
	switch mode {
	case "session":
		return streamWorker(in, out)
	case "tool":
		var request localtools.Request
		d := json.NewDecoder(io.LimitReader(in, 1<<20))
		d.DisallowUnknownFields()
		if err := d.Decode(&request); err != nil {
			return err
		}
		output, err := localtools.Execute(context.Background(), request.Call, localtools.Options{WorkingDir: "/workspace", ShellTimeout: request.ShellTimeout})
		response := localtools.Response{Output: output}
		if err != nil {
			response.Error = localtools.Limit(err.Error())
		}
		return json.NewEncoder(out).Encode(response)
	case "extract":
		return Extract("/image.tar", "/target")
	case "remove":
		entries, err := os.ReadDir("/target")
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err = os.RemoveAll(filepath.Join("/target", entry.Name())); err != nil {
				return err
			}
		}
		return nil
	case "inbox-put":
		return inboxPutWorker(in, out)
	case "inbox-clear":
		return inboxClearWorker(in, out)
	default:
		return fmt.Errorf("unknown worker operation")
	}
}

type inboxPutRequest struct {
	Name     string `json:"name"`
	MaxBytes int64  `json:"max_bytes"`
}

type inboxPutResponse struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

type inboxClearRequest struct {
	CutoffUnix int64 `json:"cutoff_unix"`
}

type inboxClearResponse struct {
	Removed int   `json:"removed"`
	Freed   int64 `json:"freed"`
}

// inboxPutWorker copies /input.bin (ro-bound host staging file) into
// /workspace/inbox with quota, dedup, and containment enforced inside the
// namespace where the directory is writable. Stdin carries only the small
// JSON header so 20 MB payloads never traverse the 1 MB tool-request path.
func inboxPutWorker(in io.Reader, out io.Writer) error {
	var req inboxPutRequest
	d := json.NewDecoder(io.LimitReader(in, 1<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(&req); err != nil {
		return err
	}
	if err := validateInboxName(req.Name); err != nil {
		return err
	}
	if req.MaxBytes <= 0 || req.MaxBytes > 1<<30 {
		return fmt.Errorf("invalid inbox quota")
	}
	data, err := os.ReadFile("/input.bin")
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return fmt.Errorf("empty inbox input")
	}
	// Defense in depth: the host DownloadFile path already caps at 20 MB.
	if int64(len(data)) > 20<<20 {
		return fmt.Errorf("inbox input exceeds 20 MB limit")
	}
	saved, err := writeInboxData("/workspace/inbox", req.Name, data, req.MaxBytes)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(inboxPutResponse{Name: saved, Size: int64(len(data))})
}

// inboxClearWorker removes inbox entries older than the cutoff inside the
// namespace. The host cannot delete sandbox-owned files when the inbox
// directory itself is owned by the subordinate UID, so deletion must also
// run where directory write permission exists.
func inboxClearWorker(in io.Reader, out io.Writer) error {
	var req inboxClearRequest
	d := json.NewDecoder(io.LimitReader(in, 1<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(&req); err != nil {
		return err
	}
	if req.CutoffUnix <= 0 {
		return fmt.Errorf("invalid inbox cutoff")
	}
	cutoff := time.Unix(req.CutoffUnix, 0)
	// Missing inbox is a no-op so chats without uploads cost nothing.
	if st, err := os.Lstat("/workspace/inbox"); err != nil {
		if os.IsNotExist(err) {
			return json.NewEncoder(out).Encode(inboxClearResponse{})
		}
		return err
	} else if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("inbox is not a directory")
	}
	removed, freed := clearInboxEntries("/workspace/inbox", cutoff)
	return json.NewEncoder(out).Encode(inboxClearResponse{Removed: removed, Freed: freed})
}
func parentsSafe(root, name string) error {
	path := filepath.Dir(name)
	for path != "." && path != "/" {
		st, err := os.Lstat(filepath.Join(root, path))
		if err != nil {
			return err
		}
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("archive parent is not a directory: %s", path)
		}
		path = filepath.Dir(path)
	}
	return nil
}
func cleanName(name string) (string, error) {
	if strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("absolute archive path")
	}
	name = filepath.Clean(name)
	if name == ".." || strings.HasPrefix(name, "../") {
		return "", fmt.Errorf("archive path escapes root")
	}
	return name, nil
}
func fileMode(mode int64) os.FileMode {
	m := os.FileMode(mode & 0777)
	if mode&04000 != 0 {
		m |= os.ModeSetuid
	}
	if mode&02000 != 0 {
		m |= os.ModeSetgid
	}
	if mode&01000 != 0 {
		m |= os.ModeSticky
	}
	return m
}

// Extract restores numeric Debian ownership within the subordinate-ID map.
// Parents may never be symlinks, even for a trusted operator-built archive.
func Extract(archive, root string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	reader := tar.NewReader(f)
	type directory struct {
		path   string
		header tar.Header
	}
	var dirs []directory
	for {
		h, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name, err := cleanName(h.Name)
		if err != nil {
			return err
		}
		if h.Uid < 0 || h.Gid < 0 || h.Uid > 65535 || h.Gid > 65535 {
			return fmt.Errorf("archive ownership is outside Debian ID range")
		}
		path := filepath.Join(root, name)
		if name != "." {
			if err = parentsSafe(root, name); err != nil {
				return err
			}
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if st, e := os.Lstat(path); e == nil {
				if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
					return fmt.Errorf("archive directory collision")
				}
			} else if e = os.Mkdir(path, 0755); e != nil {
				return e
			}
			dirs = append(dirs, directory{path, *h})
			continue
		case tar.TypeReg, tar.TypeRegA:
			dest, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if e != nil {
				return e
			}
			_, e = io.Copy(dest, reader)
			if e == nil {
				e = dest.Sync()
			}
			closeErr := dest.Close()
			if e != nil {
				return e
			}
			if closeErr != nil {
				return closeErr
			}
		case tar.TypeSymlink:
			if err = os.Symlink(h.Linkname, path); err != nil {
				return err
			}
		case tar.TypeLink:
			target, e := cleanName(h.Linkname)
			if e != nil {
				return e
			}
			if e = parentsSafe(root, target); e != nil {
				return e
			}
			st, e := os.Lstat(filepath.Join(root, target))
			if e != nil {
				return e
			}
			if !st.Mode().IsRegular() {
				return fmt.Errorf("hardlink target must be regular")
			}
			if e = os.Link(filepath.Join(root, target), path); e != nil {
				return e
			}
		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			// /dev is always replaced by bubblewrap's fresh device mount.
			if name == "dev" || strings.HasPrefix(name, "dev/") {
				continue
			}
			return fmt.Errorf("unsupported special file %s", name)
		default:
			return fmt.Errorf("unsupported tar entry %s", name)
		}
		if err = os.Lchown(path, h.Uid, h.Gid); err != nil {
			return err
		}
		if h.Typeflag != tar.TypeSymlink {
			if err = os.Chmod(path, fileMode(h.Mode)); err != nil {
				return err
			}
			if err = os.Chtimes(path, h.ModTime, h.ModTime); err != nil {
				return err
			}
		}
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		d := dirs[i]
		if err = os.Chown(d.path, d.header.Uid, d.header.Gid); err != nil {
			return err
		}
		if err = os.Chmod(d.path, fileMode(d.header.Mode)); err != nil {
			return err
		}
		if err = os.Chtimes(d.path, d.header.ModTime, d.header.ModTime); err != nil {
			return err
		}
	}
	for _, dir := range []string{"workspace", "home/agent", "proc", "dev", "tmp", "run"} {
		path := filepath.Join(root, dir)
		if err = os.MkdirAll(path, 0755); err != nil {
			return err
		}
		if err = parentsSafe(root, filepath.Join(dir, "sentinel")); err != nil {
			return err
		}
	}
	if err = os.Chmod(filepath.Join(root, "tmp"), 01777|os.ModeSticky); err != nil {
		return err
	}
	// Keep mount destinations regular and initially present in the image.
	return os.WriteFile(filepath.Join(root, ".q-agent-worker"), nil, 0555)
}
