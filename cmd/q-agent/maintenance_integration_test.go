//go:build linux

package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"q-agent/internal/config"
	"q-agent/internal/maintenance"
	"q-agent/internal/redaction"
	"q-agent/internal/sandbox"
	"q-agent/internal/service"
	"q-agent/internal/state"
	"q-agent/internal/telegram"
	"q-agent/internal/update"
	"q-agent/internal/workspace"
)

// These entry points exist only in the test executable. Production binaries do
// not accept fixture URLs, remap service names, or simulate startup failures.
func TestMain(m *testing.M) {
	var err error
	if len(os.Args) == 2 && os.Args[1] == "version" {
		fmt.Println(version)
		os.Exit(0)
	} else if len(os.Args) > 1 && os.Args[1] == "worker" {
		err = mainRun()
	} else if path := os.Getenv("Q_AGENT_LIFECYCLE_FIXTURE"); path != "" {
		err = lifecycleProbe(path)
	} else {
		os.Exit(m.Run())
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

type lifecycleFixture struct {
	Config, Unit, TelegramURL, ReleaseURL string
	FailNewBoot                           bool
}

func lifecycleProbe(path string) error {
	var fixture lifecycleFixture
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		return err
	}
	if !strings.HasPrefix(fixture.Unit, "q-agent-lifecycle-probe-") || !strings.HasSuffix(fixture.Unit, ".service") {
		return fmt.Errorf("refusing a non-probe service")
	}
	c, err := config.Load(fixture.Config)
	if err != nil {
		return err
	}
	management := &maintenance.Controller{StateDir: c.StateDir, ReadyTimeout: 3 * time.Second}
	management.Command = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		var mapped []string
		for _, arg := range args {
			if name == "systemd-run" && arg == "--" {
				mapped = append(mapped, "--setenv=Q_AGENT_LIFECYCLE_FIXTURE="+path)
			}
			mapped = append(mapped, strings.ReplaceAll(arg, "q-agent.service", fixture.Unit))
		}
		return exec.CommandContext(ctx, name, mapped...).CombinedOutput()
	}
	management.Update = func(ctx context.Context, o update.Options) error {
		o.ReleaseURL = fixture.ReleaseURL
		o.DownloadURLValidator = func(raw string) error {
			if raw != fixture.ReleaseURL+"/asset" {
				return fmt.Errorf("unexpected fixture download URL")
			}
			return nil
		}
		return update.Run(ctx, nil, o)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if len(os.Args) == 7 && os.Args[1] == "service" && os.Args[2] == "maintain" && os.Args[3] == "--state-dir" && os.Args[4] == c.StateDir && os.Args[5] == "--id" {
		return management.Run(ctx, os.Args[6])
	}
	if len(os.Args) != 4 || os.Args[1] != "serve" || os.Args[2] != "--config" || os.Args[3] != fixture.Config {
		return fmt.Errorf("unexpected probe arguments: %v", os.Args)
	}
	if fixture.FailNewBoot && version == "v0.3.0" {
		return fmt.Errorf("intentional new-version startup failure")
	}
	unlock, err := state.Lock(c.StateDir)
	if err != nil {
		return err
	}
	defer unlock()
	policy, _ := redaction.New(redaction.Config{}, nil)
	bot := telegram.New(botToken(c))
	bot.BaseURL = fixture.TelegramURL
	return serveWithMaintenance(ctx, c, policy, bot, management)
}

// This joins the real serve loop, HTTP transports, persistent state, updater,
// independent helper, systemd cgroups and (optionally) rootless sandbox. Only
// Telegram/provider/Release endpoints and the service name are substituted.
func TestIntegrationMaintenanceLifecycle(t *testing.T) {
	if os.Getenv("Q_AGENT_LIFECYCLE_SYSTEMD_TEST") != "1" {
		t.Skip("opt-in: running systemd user manager and rootless sandbox helpers required; all endpoints are offline")
	}
	if os.Getuid() == 0 {
		t.Fatal("run the lifecycle test as a non-root service account")
	}
	buildDir := t.TempDir()
	binaries := map[string]string{}
	for _, tag := range []string{"v0.2.0", "v0.3.0"} {
		binary := filepath.Join(buildDir, tag)
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		cmd := exec.CommandContext(ctx, "go", "test", "-c", "-trimpath", "-ldflags=-s -w -X q-agent/cmd/q-agent.version="+tag, "-o", binary, ".")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		out, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("build static %s fixture: %v\n%s", tag, err, out)
		}
		out, err = exec.Command(binary, "version").CombinedOutput()
		if err != nil || strings.TrimSpace(string(out)) != tag {
			t.Fatalf("fixture embedded version: got %q want %q: %v", out, tag, err)
		}
		binaries[tag] = binary
	}
	for _, isolated := range []bool{false, true} {
		for _, action := range []string{"restart", "update", "rollback"} {
			name := map[bool]string{false: "host", true: "sandbox"}[isolated] + "/" + action
			t.Run(name, func(t *testing.T) { testMaintenanceLifecycle(t, binaries, isolated, action) })
		}
	}
}

func testMaintenanceLifecycle(t *testing.T, binaries map[string]string, isolated bool, action string) {
	t.Helper()
	dir := t.TempDir()
	installed := filepath.Join(dir, "q-agent")
	old, err := os.ReadFile(binaries["v0.2.0"])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(installed, old, 0755); err != nil {
		t.Fatal(err)
	}
	var options sandbox.Options
	if isolated {
		options, err = sandbox.Resolve(sandbox.Options{Bubblewrap: "bwrap", NewUIDMap: "newuidmap", NewGIDMap: "newgidmap", Worker: installed})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if err := sandbox.Remove(ctx, options, filepath.Join(dir, "state")); err != nil {
				t.Error(err)
			}
		})
	}
	// Empty image plus real static worker is enough for read/write tools; no
	// package download, shell, or pre-existing Debian image is needed.
	image := filepath.Join(dir, "base.tar")
	f, err := os.Create(image)
	if err != nil {
		t.Fatal(err)
	}
	if err := tar.NewWriter(f).Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	imageData, err := os.ReadFile(image)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(imageData)
	if err := state.Atomic(image+".json", workspace.Image{Suite: "trixie", Architecture: runtime.GOARCH, SHA256: hex.EncodeToString(digest[:]), Size: int64(len(imageData))}); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var updates []map[string]any
	changed := make(chan struct{})
	messages := make(chan string, 64)
	modelCalls, downloads := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		respond := func(v any) { _ = json.NewEncoder(w).Encode(v) }
		switch filepath.Base(r.URL.Path) {
		case "getMe":
			respond(map[string]any{"ok": true, "result": map[string]any{"id": 99, "username": "lifecycle_bot"}})
		case "getMyCommands":
			respond(map[string]any{"ok": true, "result": telegram.DefaultCommands()})
		case "getUpdates":
			var input struct{ Offset int64 }
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
				return
			}
			for {
				mu.Lock()
				var pending []map[string]any
				for _, item := range updates {
					if item["update_id"].(int64) >= input.Offset {
						pending = append(pending, item)
					}
				}
				wait := changed
				mu.Unlock()
				if len(pending) != 0 {
					respond(map[string]any{"ok": true, "result": pending})
					return
				}
				select {
				case <-wait:
				case <-r.Context().Done():
					return
				}
			}
		case "sendMessage":
			var input struct{ Text string }
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
				return
			}
			messages <- input.Text
			respond(map[string]any{"ok": true, "result": true})
		case "sendChatAction", "deleteMessage":
			respond(map[string]any{"ok": true, "result": true})
		case "chat":
			var input struct {
				Messages []struct{ Role, Content string }
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil || len(input.Messages) == 0 {
				t.Errorf("provider request: %v", err)
				return
			}
			mu.Lock()
			modelCalls++
			step := modelCalls
			mu.Unlock()
			last := input.Messages[len(input.Messages)-1]
			if step == 3 || step == 5 {
				if last.Role != "tool" || !strings.Contains(last.Content, "persistent-marker") || !strings.Contains(last.Content, `"trust":"untrusted"`) {
					t.Errorf("persistent read failed at step %d: %+v", step, last)
				}
			}
			if step == 3 || step == 6 {
				respond(map[string]any{"message": map[string]any{"role": "assistant", "content": map[bool]string{true: "before-complete", false: "after-complete"}[step == 3]}, "done": true})
				return
			}
			tool := "read"
			args := map[string]string{"path": "persistent.txt"}
			if step == 1 || step == 5 {
				tool = "write"
				args["content"] = "persistent-marker"
			}
			respond(map[string]any{"message": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"function": map[string]any{"name": tool, "arguments": args}}}}, "done": true})
		case "release":
			respond(map[string]any{"tag_name": "v0.3.0", "assets": []any{map[string]any{"name": "q-agent_v0.3.0_linux_" + runtime.GOARCH + ".tar.gz", "browser_download_url": "http://" + r.Host + "/release/asset"}}})
		case "asset":
			mu.Lock()
			downloads++
			mu.Unlock()
			file, err := os.Open(binaries["v0.3.0"])
			if err != nil {
				t.Error(err)
				return
			}
			defer file.Close()
			info, err := file.Stat()
			if err != nil {
				t.Error(err)
				return
			}
			gz := gzip.NewWriter(w)
			tarball := tar.NewWriter(gz)
			if err := tarball.WriteHeader(&tar.Header{Name: "q-agent", Mode: 0755, Size: info.Size()}); err != nil {
				t.Error(err)
			}
			if _, err := io.Copy(tarball, file); err != nil {
				t.Error(err)
			}
			if err := tarball.Close(); err != nil {
				t.Error(err)
			}
			if err := gz.Close(); err != nil {
				t.Error(err)
			}
		default:
			t.Errorf("unexpected offline endpoint: %s", r.URL.Path)
			http.Error(w, "unexpected endpoint", http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	enqueue := func(id int64, text string) {
		mu.Lock()
		defer mu.Unlock()
		updates = append(updates, map[string]any{"update_id": id, "message": map[string]any{"message_id": id, "from": map[string]any{"id": 1}, "chat": map[string]any{"id": 1, "type": "private"}, "text": text}})
		close(changed)
		changed = make(chan struct{})
	}
	waitMessage := func(want string) string {
		t.Helper()
		timer := time.NewTimer(25 * time.Second)
		defer timer.Stop()
		var received []string
		for {
			select {
			case text := <-messages:
				received = append(received, text)
				if strings.Contains(text, want) {
					return text
				}
			case <-timer.C:
				t.Fatalf("timed out waiting for %q; received: %q", want, received)
			}
		}
	}

	cfg := filepath.Join(dir, "config.toml")
	configText := fmt.Sprintf("state_dir = %s\n[provider]\nname = 'ollama'\nmodel = 'offline'\nbase_url = %s\n[telegram]\ntoken_path = 'telegram.token'\nallowed_users = [1]\nallowed_groups = []\nadmin_user_ids = [1]\n[sandbox]\nenabled = %t\nbase_image = %s\n", strconv.Quote(filepath.Join(dir, "state")), strconv.Quote(server.URL), isolated, strconv.Quote(image))
	unit := fmt.Sprintf("q-agent-lifecycle-probe-%d-%d.service", os.Getpid(), time.Now().UnixNano())
	fixturePath := filepath.Join(dir, "fixture.json")
	if err := state.Atomic(fixturePath, lifecycleFixture{Config: cfg, Unit: unit, TelegramURL: server.URL, ReleaseURL: server.URL + "/release", FailNewBoot: action == "rollback"}); err != nil {
		t.Fatal(err)
	}
	for path, contents := range map[string]string{
		cfg:                                  configText,
		filepath.Join(dir, "telegram.token"): "offline-lifecycle-token\n",
		filepath.Join(dir, "service.env"):    "Q_AGENT_LIFECYCLE_FIXTURE=" + strconv.Quote(fixturePath) + "\n",
	} {
		if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var rendered strings.Builder
	if err := service.Run([]string{"print", "--config", cfg}, service.Options{Executable: installed, Stdout: &rendered}); err != nil {
		t.Fatal(err)
	}
	unitPath := filepath.Join(dir, unit)
	unitText := strings.Replace(rendered.String(), "Restart=on-failure", "Restart=no", 1)
	unitText = strings.Replace(unitText, "StandardOutput=journal", "StandardOutput=append:"+filepath.Join(dir, "probe.log"), 1)
	unitText = strings.Replace(unitText, "StandardError=journal", "StandardError=inherit", 1)
	if err := os.WriteFile(unitPath, []byte(unitText), 0600); err != nil {
		t.Fatal(err)
	}
	control := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "systemctl", append([]string{"--user"}, args...)...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	if out, err := control("show", unit, "--property=LoadState", "--value"); err != nil || out != "not-found" {
		t.Fatalf("refusing existing unit or unavailable user manager: %q %v", out, err)
	}
	t.Cleanup(func() {
		// Stop the helper before the bot: its ExecStopPost may start the bot.
		data, _ := os.ReadFile(filepath.Join(dir, "state", "maintenance", "request.json"))
		var request struct{ HelperUnit string }
		if json.Unmarshal(data, &request) == nil && strings.HasPrefix(request.HelperUnit, "q-agent-maintenance-") {
			control("stop", request.HelperUnit)
			control("reset-failed", request.HelperUnit)
		}
		control("stop", unit)
		if out, err := control("disable", "--runtime", unit); err != nil {
			t.Errorf("remove probe unit: %s %v", out, err)
		}
		control("reset-failed", unit)
		if out, err := control("daemon-reload"); err != nil {
			t.Errorf("reload after cleanup: %s %v", out, err)
		}
		if t.Failed() {
			out, _ := os.ReadFile(filepath.Join(dir, "probe.log"))
			t.Logf("probe log:\n%s", out)
		}
	})
	for _, args := range [][]string{{"link", "--runtime", unitPath}, {"daemon-reload"}, {"start", unit}} {
		if out, err := control(args...); err != nil {
			t.Fatalf("start disposable unit: %s %v", out, err)
		}
	}
	enqueue(1, "/ask create and read a persistent marker")
	waitMessage("before-complete")
	oldPID, err := control("show", unit, "--property=MainPID", "--value")
	if err != nil || oldPID == "0" {
		t.Fatalf("initial MainPID: %s %v", oldPID, err)
	}
	command := "/update"
	if action == "restart" {
		command = "/restart"
	}
	enqueue(2, command)
	waitMessage("requested")
	wantVersion := "v0.2.0"
	wantResult := "Restart completed."
	if action == "update" {
		wantVersion, wantResult = "v0.3.0", "Update completed."
	} else if action == "rollback" {
		wantResult = "Previous executable restored. Service recovered"
	}
	result := waitMessage(wantResult)
	if !strings.Contains(result, "q-agent "+wantVersion) {
		t.Fatalf("wrong running version: %s", result)
	}
	newPID, err := control("show", unit, "--property=MainPID", "--value")
	if err != nil || newPID == "0" || newPID == oldPID {
		t.Fatalf("no new MainPID: old=%s new=%s err=%v", oldPID, newPID, err)
	}
	enqueue(3, "/ask read the persistent marker and write it again")
	waitMessage("after-complete")
	mu.Lock()
	calls, fetched := modelCalls, downloads
	mu.Unlock()
	if calls != 6 || fetched != map[bool]int{true: 0, false: 1}[action == "restart"] {
		t.Fatalf("unexpected model calls/downloads: %d/%d", calls, fetched)
	}
	actual, err := os.ReadFile(installed)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := os.ReadFile(binaries[wantVersion])
	if err != nil || sha256.Sum256(actual) != sha256.Sum256(expected) {
		t.Fatalf("installed executable is not %s: %v", wantVersion, err)
	}
	for path, want := range map[string]string{cfg: configText, filepath.Join(dir, "telegram.token"): "offline-lifecycle-token\n"} {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != want {
			t.Fatalf("configuration/credential changed: %s %v", path, err)
		}
	}
	if backups, err := filepath.Glob(filepath.Join(dir, ".q-agent-backup-*")); err != nil || len(backups) != 0 {
		t.Fatalf("backup was not cleaned up: %v %v", backups, err)
	}
	t.Logf("%s: PID %s -> %s, running %s, tools before/after, release downloads=%d", action, oldPID, newPID, wantVersion, fetched)
}
