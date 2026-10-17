//go:build linux

package sandbox_test

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"q-agent/internal/llm"
	"q-agent/internal/sandbox"
)

// A minimal rootfs exercises the real static worker, namespace and ID mapping
// without downloading/installing Debian or changing any deployed bot state.
func TestIntegrationWebTools(t *testing.T) {
	if os.Getenv("Q_AGENT_WEB_INTEGRATION") != "1" {
		t.Skip("set Q_AGENT_WEB_INTEGRATION=1 and Q_AGENT_WORKER for real sandbox web checks")
	}
	options, err := sandbox.Resolve(sandbox.Options{Bubblewrap: "bwrap", NewUIDMap: "newuidmap", NewGIDMap: "newgidmap", Worker: os.Getenv("Q_AGENT_WORKER")})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	image := filepath.Join(dir, "minimal.tar")
	f, err := os.Create(image)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	if os.Getenv("Q_AGENT_WEB_LIVE_TEST") == "1" {
		for _, name := range []string{"etc", "etc/ssl", "etc/ssl/certs"} {
			if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: 0755}); err != nil {
				t.Fatal(err)
			}
		}
		for _, name := range []string{"etc/resolv.conf", "etc/ssl/certs/ca-certificates.crt"} {
			data, err := os.ReadFile("/" + name)
			if err != nil {
				t.Fatal(err)
			}
			if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0644, Size: int64(len(data))}); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write(data); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := sandbox.Remove(ctx, options, root); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := sandbox.Provision(ctx, options, image, root); err != nil {
		t.Fatal(err)
	}
	runner := &sandbox.Runner{Options: options, RootFS: root}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<title>Sandbox web</title><p>worker HTTP fetch succeeded</p>`)
	}))
	defer server.Close()
	args, _ := json.Marshal(map[string]string{"url": server.URL})
	out, err := runner.Execute(ctx, llm.ToolCall{Name: "web_fetch", Arguments: args})
	if err != nil || !strings.Contains(out, "worker HTTP fetch succeeded") {
		t.Fatalf("sandbox fetch: %s %v", out, err)
	}
	if _, err := runner.Execute(ctx, llm.ToolCall{Name: "web_search", Arguments: json.RawMessage(`{"query":"test","count":0}`)}); err == nil || !strings.Contains(err.Error(), "count") {
		t.Fatalf("sandbox search validation: %v", err)
	}
	if os.Getenv("Q_AGENT_WEB_LIVE_TEST") == "1" {
		out, err := runner.Execute(ctx, llm.ToolCall{Name: "web_search", Arguments: json.RawMessage(`{"query":"Go programming language","count":3}`)})
		if err != nil || !strings.Contains(out, "https://go.dev/") {
			t.Fatalf("sandbox live search: %s %v", out, err)
		}
		t.Log(out)
		out, err = runner.Execute(ctx, llm.ToolCall{Name: "web_fetch", Arguments: json.RawMessage(`{"url":"https://go.dev/"}`)})
		if err != nil || !strings.Contains(out, "Go Programming Language") {
			t.Fatalf("sandbox live HTTPS: %.400s %v", out, err)
		}
		t.Logf("sandbox HTTPS fetch: %d bytes", len(out))
	}
}
