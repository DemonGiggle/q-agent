//go:build linux

package process

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/term"
)

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
func TestDeviceAuthorizationFixture(t *testing.T) {
	var approved atomic.Bool
	browser := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/authorize" {
			approved.Store(true)
			fmt.Fprint(w, "approved")
			return
		}
		if !approved.Load() {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		fmt.Fprint(w, "authorized")
	}))
	defer browser.Close()
	dir := t.TempDir()
	m := New(context.Background(), HostLauncher(dir, []string{"PATH=/usr/bin:/bin"}), nil, Hooks{})
	defer m.Close()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	r := start(t, m, "Q_AGENT_DEVICE_FIXTURE="+shellQuote(browser.URL)+" "+shellQuote(exe)+" -test.run=^TestDeviceApplicationFixture$", true)
	until(t, m, r.ID, func(r Result) bool { return strings.Contains(r.Output, "Browser") })
	screen := request(t, m, Request{Action: "read", ID: r.ID, View: "screen"}).Screen
	if !screen.Alternate || len(screen.Emphasis) == 0 {
		t.Fatalf("menu screen: %+v", screen)
	}
	request(t, m, Request{Action: "keys", ID: r.ID, Keys: []string{"Down"}})
	until(t, m, r.ID, func(r Result) bool { return strings.Count(r.Output, "Browser") >= 2 })
	screen = request(t, m, Request{Action: "read", ID: r.ID, View: "screen"}).Screen
	selected := false
	for _, e := range screen.Emphasis {
		if e.Row == 1 && e.Reverse {
			selected = true
		}
	}
	if !selected {
		t.Fatal("arrow key did not select browser authorization")
	}
	request(t, m, Request{Action: "keys", ID: r.ID, Keys: []string{"Enter"}})
	r = until(t, m, r.ID, func(r Result) bool { return strings.Contains(r.Output, "ABCD-1234") })
	if !strings.Contains(r.Output, browser.URL) {
		t.Fatal("authorization URL unavailable")
	}
	response, err := http.Get(browser.URL + "/authorize")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	r = until(t, m, r.ID, func(r Result) bool { return r.Status == "exited" })
	if *r.ExitCode != 0 || !strings.Contains(r.Output, "Logged in fixture-user") {
		t.Fatal(r)
	}
	// The fixture writes credentials; the verification command checks the saved
	// authentication state without printing the token, as gh auth status would.
	r = start(t, m, "test -s authorization.fixture && printf 'authenticated fixture-user'", false)
	r = until(t, m, r.ID, func(r Result) bool { return r.Status == "exited" })
	if *r.ExitCode != 0 || !strings.Contains(r.Output, "authenticated fixture-user") {
		t.Fatal(r)
	}
}
func TestDeviceApplicationFixture(t *testing.T) {
	endpoint := os.Getenv("Q_AGENT_DEVICE_FIXTURE")
	if endpoint == "" {
		return
	}
	previous, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	defer term.Restore(int(os.Stdin.Fd()), previous)
	fmt.Print("\x1b[6n")
	report := make([]byte, 6)
	if _, err = io.ReadFull(os.Stdin, report); err != nil || string(report) != "\x1b[1;1R" {
		t.Fatalf("cursor response: %q %v", report, err)
	}
	fmt.Print("\x1b[?1049h\x1b[H\x1b[7mToken\x1b[0m\r\nBrowser")
	arrow := make([]byte, 3)
	if _, err = io.ReadFull(os.Stdin, arrow); err != nil {
		t.Fatal(err)
	}
	if string(arrow) != "\x1b[B" {
		t.Fatal("expected Down")
	}
	fmt.Print("\x1b[H\x1b[0mToken\r\n\x1b[7mBrowser\x1b[0m")
	enter := make([]byte, 1)
	if _, err = io.ReadFull(os.Stdin, enter); err != nil {
		t.Fatal(err)
	}
	if enter[0] != '\r' {
		t.Fatal("expected Enter")
	}
	fmt.Printf("\x1b[?1049lOpen %s/authorize and enter ABCD-1234", endpoint)
	for i := 0; i < 200; i++ {
		response, err := http.Get(endpoint + "/token")
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode == http.StatusOK {
			if err = os.WriteFile("authorization.fixture", []byte("fixture-credential"), 0600); err != nil {
				t.Fatal(err)
			}
			fmt.Print("\r\nLogged in fixture-user\r\n")
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("device authorization timed out")
}

// Opt-in checks against installed applications. HOME/GH_CONFIG_DIR are isolated;
// gh is cancelled after its initial browser-login prompt and never stores auth.
func TestLiveInteractiveApps(t *testing.T) {
	if os.Getenv("Q_AGENT_PROCESS_LIVE_TEST") != "1" {
		t.Skip("set Q_AGENT_PROCESS_LIVE_TEST=1 for installed Python, Vim and gh checks")
	}
	dir := t.TempDir()
	environment := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "GH_CONFIG_DIR=" + filepath.Join(dir, "gh"), "LANG=C.UTF-8", "BROWSER=/bin/true"}
	m := New(context.Background(), HostLauncher(dir, environment), nil, Hooks{})
	defer m.Close()
	for _, binary := range []string{"python3", "vim", "gh"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Fatal(err)
		}
	}
	r := start(t, m, "python3 -q", true)
	until(t, m, r.ID, func(r Result) bool { return strings.Contains(r.Output, ">>>") })
	request(t, m, Request{Action: "write", ID: r.ID, Data: "print(6 * 7)"})
	request(t, m, Request{Action: "keys", ID: r.ID, Keys: []string{"Enter"}})
	until(t, m, r.ID, func(r Result) bool { return strings.Contains(r.Output, "42") })
	request(t, m, Request{Action: "keys", ID: r.ID, Keys: []string{"Ctrl-D"}})
	until(t, m, r.ID, func(r Result) bool { return r.Status == "exited" })
	r = start(t, m, "vim -Nu NONE -n -i NONE smoke.txt", true)
	until(t, m, r.ID, func(r Result) bool { return strings.Contains(r.Output, "smoke.txt") })
	screen := request(t, m, Request{Action: "read", ID: r.ID, View: "screen"}).Screen
	if screen == nil {
		t.Fatal("vim screen unavailable")
	}
	request(t, m, Request{Action: "write", ID: r.ID, Data: "iinteractive smoke"})
	request(t, m, Request{Action: "keys", ID: r.ID, Keys: []string{"Escape"}})
	request(t, m, Request{Action: "write", ID: r.ID, Data: ":wq"})
	request(t, m, Request{Action: "keys", ID: r.ID, Keys: []string{"Enter"}})
	until(t, m, r.ID, func(r Result) bool { return r.Status == "exited" })
	if data, err := os.ReadFile(filepath.Join(dir, "smoke.txt")); err != nil || !strings.Contains(string(data), "interactive smoke") {
		t.Fatalf("vim write: %s %v", data, err)
	}
	r = start(t, m, "gh auth login --hostname github.com --git-protocol https --web", true)
	r = until(t, m, r.ID, func(r Result) bool {
		return strings.Contains(r.Output, "?") || strings.Contains(r.Output, "code") || r.Status == "exited"
	})
	if r.Status == "exited" {
		t.Fatal("gh login failed to reach interactive prompt")
	}
	// Allow gh to finish installing its raw-mode input handler.
	time.Sleep(100 * time.Millisecond)
	request(t, m, Request{Action: "keys", ID: r.ID, Keys: []string{"Ctrl-C"}})
	until(t, m, r.ID, func(r Result) bool { return r.Status == "exited" })
	r = start(t, m, "gh auth status --hostname github.com", false)
	r = until(t, m, r.ID, func(r Result) bool { return r.Status == "exited" })
	if *r.ExitCode == 0 || !strings.Contains(r.Output, "not logged") {
		t.Fatal("unexpected state in isolated gh home: " + r.Output)
	}
}
