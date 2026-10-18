package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func archiveBytes(t *testing.T, headers ...*tar.Header) []byte {
	t.Helper()
	var b bytes.Buffer
	z := gzip.NewWriter(&b)
	w := tar.NewWriter(z)
	for _, h := range headers {
		if err := w.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := io.CopyN(w, strings.NewReader(strings.Repeat("x", int(h.Size))), h.Size); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestArchiveUpdaterInstallsOnlyExecutable(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "q-agent")
	if err := os.WriteFile(target, []byte("old"), 0751); err != nil {
		t.Fatal(err)
	}
	body := archiveBytes(t, &tar.Header{Name: "q-agent", Mode: 0755, Size: 20, Typeflag: tar.TypeReg}, &tar.Header{Name: "../config.toml", Mode: 0600, Size: 4, Typeflag: tar.TypeReg})
	meta, _ := json.Marshal(release{TagName: "v0.1.0", Assets: []asset{{Name: "q-agent_v0.1.0_linux_amd64.tar.gz", BrowserDownloadURL: "https://test.invalid/archive"}}})
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/latest" {
			return fakeResponse(http.StatusOK, string(meta)), nil
		}
		return fakeResponse(http.StatusOK, string(body)), nil
	})}
	err := Run(context.Background(), nil, Options{CurrentVersion: "dev", GOOS: "linux", GOARCH: "amd64", ExecutablePath: target, Client: client, ReleaseURL: "https://test.invalid/latest", DownloadURLValidator: func(string) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(target)
	info, _ := os.Stat(target)
	if string(data) != strings.Repeat("x", 20) || info.Mode().Perm() != 0751 {
		t.Fatalf("%q mode %o", data, info.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("archive extracted other entries or leaked temp files: %v", entries)
	}
}

func TestArchiveFailuresAndCleanup(t *testing.T) {
	regular := func(name string) *tar.Header {
		return &tar.Header{Name: name, Mode: 0755, Size: 5, Typeflag: tar.TypeReg}
	}
	corruptChecksum := archiveBytes(t, regular("q-agent"))
	corruptChecksum[len(corruptChecksum)-8] ^= 1
	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"missing executable", archiveBytes(t, regular("README.md"))},
		{"nested executable", archiveBytes(t, regular("bin/q-agent"))},
		{"duplicate", archiveBytes(t, regular("q-agent"), regular("./q-agent"))},
		{"symlink", archiveBytes(t, &tar.Header{Name: "q-agent", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"})},
		{"empty", archiveBytes(t, &tar.Header{Name: "q-agent", Typeflag: tar.TypeReg})},
		{"not gzip", []byte("not gzip")},
		{"truncated gzip", archiveBytes(t, regular("q-agent"))[:30]},
		{"corrupt checksum after executable", corruptChecksum},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "archive")
			if err := os.WriteFile(path, tc.body, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := unpackArchive(path, 0755); err == nil {
				t.Fatal("invalid archive accepted")
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 1 {
				t.Fatalf("temporary file leaked: %v", entries)
			}
		})
	}
}

func TestReleaseAssetContractAndELFGuard(t *testing.T) {
	assets := []asset{{Name: "q-agent_v1.0.0_linux_arm64.tar.gz"}, {Name: "q-agent_linux_arm64"}}
	if a, ok := findReleaseAsset(assets, "v1.0.0", "linux", "arm64"); !ok || a.Name != "q-agent_linux_arm64" {
		t.Fatal(a)
	}
	if _, ok := findReleaseAsset(assets[:1], "v1.1.0", "linux", "arm64"); ok {
		t.Fatal("wrong release archive selected")
	}
	path := filepath.Join(t.TempDir(), "bad")
	if err := os.WriteFile(path, []byte("not executable"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := validateELF(path, "amd64"); err == nil {
		t.Fatal("invalid binary accepted")
	}
}
