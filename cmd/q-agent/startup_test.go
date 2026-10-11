//go:build linux

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"q-agent/internal/config"
	"q-agent/internal/state"
	"q-agent/internal/telegram"
	"q-agent/internal/workspace"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

func startupConfig(t *testing.T) config.Config {
	t.Helper()
	t.Setenv("TELEGRAM_BOT_TOKEN", "startup-test-credential")
	c := config.Defaults()
	c.StateDir = t.TempDir()
	c.Telegram.ApprovedUsers = []int64{1}
	c.Telegram.ApprovedChats = []int64{1}
	return c
}

func TestServeSandboxSetupFailure(t *testing.T) {
	for _, kind := range []string{"missing image", "missing dependencies", "invalid worker"} {
		t.Run(kind, func(t *testing.T) {
			c := startupConfig(t)
			c.Sandbox.Enabled = true
			c.Sandbox.BaseImage = filepath.Join(c.StateDir, "base.tar")
			c.Sandbox.Worker = filepath.Join(c.StateDir, "missing-worker")
			if kind != "missing image" {
				data := []byte("test image")
				if err := os.WriteFile(c.Sandbox.BaseImage, data, 0600); err != nil {
					t.Fatal(err)
				}
				digest := sha256.Sum256(data)
				if err := state.Atomic(c.Sandbox.BaseImage+".json", workspace.Image{Suite: "trixie", Architecture: runtime.GOARCH, SHA256: hex.EncodeToString(digest[:]), Size: int64(len(data))}); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "missing dependencies" {
				c.Sandbox.Bubblewrap = filepath.Join(c.StateDir, "missing-bwrap")
			}
			// Telegram access would indicate that setup failed to stop startup.
			var networkCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				networkCalls.Add(1)
				fmt.Fprintln(w, `{"ok":true,"result":{"id":99,"username":"test_bot"}}`)
			}))
			defer server.Close()
			bot := telegram.New("startup-test-credential")
			bot.BaseURL = server.URL
			err := serve(context.Background(), c, nil, bot)
			if err == nil || networkCalls.Load() != 0 {
				t.Fatalf("invalid sandbox started: %v calls=%d", err, networkCalls.Load())
			}
			for _, name := range []string{"host-workspaces", "execution.json"} {
				if _, err := os.Stat(filepath.Join(c.StateDir, name)); !os.IsNotExist(err) {
					t.Fatal("sandbox setup fell back to host")
				}
			}
		})
	}
}

func TestServeModeResetFailureStopsBeforeNetworkWork(t *testing.T) {
	c := startupConfig(t)
	s, err := state.Open(c.StateDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save("legacy-topic", state.Conversation{Identity: "old-identity", Pairs: []state.Pair{{User: "old"}}}); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(c.StateDir, "conversations", "z.json"), 0700); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprintln(w, `{"ok":true,"result":{"id":99,"username":"test_bot"}}`)
	}))
	defer server.Close()
	bot := telegram.New("startup-test-credential")
	bot.BaseURL = server.URL
	for attempt := 0; attempt < 2; attempt++ {
		err := serve(context.Background(), c, nil, bot)
		if err == nil || !strings.Contains(err.Error(), "execution mode") || calls.Load() != 0 {
			t.Fatalf("failed reset started network work: %v calls=%d", err, calls.Load())
		}
		if _, err := os.Stat(filepath.Join(c.StateDir, "execution.json")); !os.IsNotExist(err) {
			t.Fatal("failed reset committed the new mode")
		}
	}
}
