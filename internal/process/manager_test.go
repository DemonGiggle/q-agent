//go:build linux

package process

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"q-agent/internal/redaction"
)

func testManager(t *testing.T, hooks Hooks) *Manager {
	t.Helper()
	m := New(context.Background(), HostLauncher(t.TempDir(), []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}), nil, hooks)
	t.Cleanup(m.Close)
	return m
}
func result(t *testing.T, out string, err error) Result {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > ResultLimit {
		t.Fatal("encoded result exceeds bound")
	}
	var r Result
	if err = json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("invalid JSON: %v %s", err, out)
	}
	return r
}
func start(t *testing.T, m *Manager, command string, pty bool) Result {
	t.Helper()
	out, err := m.Start(context.Background(), Spec{Command: command, PTY: pty}, 20*time.Millisecond)
	return result(t, out, err)
}
func request(t *testing.T, m *Manager, a Request) Result {
	t.Helper()
	out, err := m.Execute(context.Background(), a)
	return result(t, out, err)
}
func until(t *testing.T, m *Manager, id string, predicate func(Result) bool) Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var r Result
	for ctx.Err() == nil {
		out, err := m.Execute(ctx, Request{Action: "read", ID: id})
		r = result(t, out, err)
		if predicate(r) {
			return r
		}
		cursor := r.EndCursor
		wait := 100
		_, err = m.Execute(ctx, Request{Action: "wait", ID: id, Cursor: cursor, TimeoutMS: &wait})
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Fatalf("session did not reach expected state: %+v", r)
	return r
}
func TestPTYPromptKeysResizeAndEOF(t *testing.T) {
	m := testManager(t, Hooks{})
	r := start(t, m, `test -t 0 && test -t 1 && test -r /dev/tty || exit 9; printf 'Name? '; read name; printf '\nHello %s 😀中\n' "$name"; stty size; cat`, true)
	if !strings.Contains(r.Output, "Name? ") {
		r = until(t, m, r.ID, func(r Result) bool { return strings.Contains(r.Output, "Name? ") })
	}
	request(t, m, Request{Action: "resize", ID: r.ID, Rows: 30, Columns: 100})
	request(t, m, Request{Action: "write", ID: r.ID, Data: "friend"})
	request(t, m, Request{Action: "keys", ID: r.ID, Keys: []string{"Enter"}})
	r = until(t, m, r.ID, func(r Result) bool { return strings.Contains(r.Output, "30 100") })
	if !strings.Contains(r.Output, "Hello friend 😀中") {
		t.Fatal(r.Output)
	}
	screen := request(t, m, Request{Action: "read", ID: r.ID, View: "screen"}).Screen
	if screen.Columns != 100 || screen.Rows != 30 {
		t.Fatalf("resize: %+v", screen)
	}
	request(t, m, Request{Action: "keys", ID: r.ID, Keys: []string{"Ctrl-D"}})
	r = until(t, m, r.ID, func(r Result) bool { return r.Status == "exited" })
	if *r.ExitCode != 0 {
		t.Fatalf("exit: %+v", r)
	}
}
func TestPTYCtrlCAndNormalApplicationFailure(t *testing.T) {
	m := testManager(t, Hooks{})
	r := start(t, m, `printf ready; sleep 30`, true)
	until(t, m, r.ID, func(r Result) bool { return strings.Contains(r.Output, "ready") })
	request(t, m, Request{Action: "keys", ID: r.ID, Keys: []string{"Ctrl-C"}})
	until(t, m, r.ID, func(r Result) bool { return r.Status == "exited" })
	r = start(t, m, "printf failed; exit 42", false)
	r = until(t, m, r.ID, func(r Result) bool { return r.Status == "exited" })
	if *r.ExitCode != 42 {
		t.Fatal(r)
	}
}
func TestLimitsDuplicateLaunchAndTaskIsolation(t *testing.T) {
	m := testManager(t, Hooks{})
	a := start(t, m, "sleep 30", false)
	if _, err := m.Start(context.Background(), Spec{Command: "sleep 30"}, 0); err == nil {
		t.Fatal("duplicate launch accepted")
	}
	b := start(t, m, "sleep 31", true)
	if _, err := m.Start(context.Background(), Spec{Command: "sleep 32"}, 0); err == nil {
		t.Fatal("session limit ignored")
	}
	other := testManager(t, Hooks{})
	if _, err := other.Execute(context.Background(), Request{Action: "read", ID: a.ID}); err == nil {
		t.Fatal("cross-task ID accepted")
	}
	request(t, m, Request{Action: "terminate", ID: a.ID})
	c := start(t, m, "printf replacement", false)
	if c.ID == a.ID || c.ID == b.ID {
		t.Fatal("session ID reused")
	}
	out, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	var list struct{ Sessions []Result }
	_ = json.Unmarshal([]byte(out), &list)
	if len(list.Sessions) > 2 {
		t.Fatal("unbounded completed sessions")
	}
	m.Close()
}
func TestBoundedHistoryEncodedPaginationAndRedaction(t *testing.T) {
	m := testManager(t, Hooks{})
	secret := "ghp_" + strings.Repeat("q", 36)
	r := start(t, m, `printf 'ghp_'; read gate; printf '`+strings.Repeat("q", 36)+`\n'; head -c 300000 /dev/zero | tr '\000' x; printf '\n😀\"\\\n'`, false)
	if strings.Contains(r.Output, "ghp_") {
		t.Fatal("partial token exposed")
	}
	request(t, m, Request{Action: "write", ID: r.ID, Data: "go\n"})
	r = until(t, m, r.ID, func(r Result) bool { return r.Status == "exited" })
	s, _ := m.get(r.ID)
	s.mu.Lock()
	retained := len(s.history)
	s.mu.Unlock()
	if retained > HistoryLimit || r.HistoryStart == 0 || !r.Truncated {
		t.Fatalf("history bound: %d %+v", retained, r)
	}
	cursor := int64(0)
	var output strings.Builder
	for {
		page := request(t, m, Request{Action: "read", ID: r.ID, Cursor: cursor})
		if !utf8.ValidString(page.Output) || strings.Contains(page.Output, secret) {
			t.Fatal("unsafe output")
		}
		output.WriteString(page.Output)
		if page.NextCursor == page.EndCursor {
			break
		}
		if page.NextCursor <= cursor {
			t.Fatal("pagination stalled")
		}
		cursor = page.NextCursor
	}
	if !strings.Contains(output.String(), "😀") {
		t.Fatal("UTF-8 lost")
	}
}
func TestWaitWakeInterimAndScheduledRejection(t *testing.T) {
	wake := make(chan struct{}, 1)
	interim := make(chan string, 1)
	m := testManager(t, Hooks{Wake: wake, HumanAvailable: true, Interim: func(s string) error { interim <- s; return nil }})
	r := start(t, m, "sleep 30", false)
	done := make(chan Result, 1)
	go func() {
		out, err := m.Execute(context.Background(), Request{Action: "wait", ID: r.ID, Human: true, Message: "Open https://example.test/device code ABCD-1234 password=hidden"})
		if err != nil {
			done <- Result{Reason: err.Error()}
			return
		}
		var v Result
		_ = json.Unmarshal([]byte(out), &v)
		done <- v
	}()
	select {
	case msg := <-interim:
		if strings.Contains(msg, "hidden") {
			t.Fatal("interim secret exposed")
		}
	case <-time.After(time.Second):
		t.Fatal("interim not sent")
	}
	wake <- struct{}{}
	select {
	case v := <-done:
		if v.Reason != "user_input" {
			t.Fatal(v)
		}
	case <-time.After(time.Second):
		t.Fatal("wait did not wake")
	}
	scheduled := testManager(t, Hooks{})
	if _, err := scheduled.Execute(context.Background(), Request{Action: "wait", Human: true}); err == nil || !strings.Contains(err.Error(), "human input unavailable") {
		t.Fatal(err)
	}
}
func TestServerAndDescendantsEndWithTask(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	m := New(ctx, HostLauncher(dir, nil), nil, Hooks{})
	defer m.Close()
	// Use the test binary as a deterministic HTTP fixture, without Python.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := fmt.Sprintf("Q_AGENT_PROCESS_SERVER=%s '%s' -test.run=^TestProcessServerFixture$", address, exe)
	r := start(t, m, command, false)
	until(t, m, r.ID, func(r Result) bool { return strings.Contains(r.Output, "server-ready") })
	response, err := http.Get("http://" + address)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if string(body) != "alive" {
		t.Fatal(string(body))
	}
	child := start(t, m, "(sleep 1; touch late) & wait", false)
	if child.Status != "running" {
		t.Fatal(child)
	}
	cancel()
	m.Close()
	client := http.Client{Timeout: 100 * time.Millisecond}
	if response, err = client.Get("http://" + address); err == nil {
		response.Body.Close()
		t.Fatal("server survived task")
	}
	time.Sleep(1100 * time.Millisecond)
	if _, err = os.Stat(filepath.Join(dir, "late")); !os.IsNotExist(err) {
		t.Fatal("descendant survived task cleanup")
	}
}
func TestProcessServerFixture(t *testing.T) {
	address := os.Getenv("Q_AGENT_PROCESS_SERVER")
	if address == "" {
		return
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Print("server-ready")
	_ = http.Serve(listener, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "alive") }))
}

func TestScreenRedrawAlternateEmphasisAndFragmentedSecrets(t *testing.T) {
	secret := "configured-sensitive-value"
	policy, _ := redaction.New(redaction.Config{}, []string{secret})
	s := newScreen(4, 20)
	s.feed([]byte("old\rnew\x1b[K\n\x1b[?1049h\x1b[H\x1b[7mChoice A\x1b[0m\nChoice B"))
	snap := s.snapshot(policy)
	if !snap.Alternate || !strings.Contains(strings.Join(snap.Lines, "\n"), "Choice A") || len(snap.Emphasis) == 0 || !snap.Emphasis[0].Reverse {
		t.Fatalf("menu: %+v", snap)
	}
	for _, chunk := range []string{"\x1b[3;1Hconfigured-", "\x1b[31msensitive-", "value", "😀中"} {
		s.feed([]byte(chunk))
	}
	if text := strings.Join(s.snapshot(policy).Lines, "\n"); strings.Contains(text, "configured") || strings.Contains(text, "sensitive") || strings.Contains(text, "value") {
		t.Fatal("screen credential leaked: " + text)
	}
	s.feed([]byte("\x1b[?1049l"))
	snap = s.snapshot(policy)
	if snap.Alternate || !strings.HasPrefix(snap.Lines[0], "new") {
		t.Fatalf("main screen not restored: %+v", snap)
	}
	s.feed([]byte("\x1b[2J\x1b[H"))
	s.feed([]byte{0xf0, 0x9f})
	s.feed([]byte{0x98, 0x80})
	if !strings.HasPrefix(s.snapshot(policy).Lines[0], "😀") {
		t.Fatal("fragmented UTF-8 lost")
	}
	// Oversized/incomplete terminal control data cannot grow unbounded state.
	s.feed([]byte("\x1b]" + strings.Repeat("x", 10000)))
	if len(s.pending) > 4096 {
		t.Fatal("unbounded escape state")
	}
}

func TestScreenPaginationAndControlBounds(t *testing.T) {
	s := newScreen(120, 240)
	s.feed([]byte("\x1b[38;5;99999m\x1b[999999999S\x1b[Hsafe\x1b]" + strings.Repeat("x", 10000)))
	s.feed([]byte(strings.Repeat("x", 10000) + "\x1b\\"))
	if text := strings.Join(s.snapshot(nil).Lines, ""); strings.Contains(text, "xxxx") {
		t.Fatal("oversized OSC exposed as screen text")
	}
	var p plain
	if out := string(p.feed([]byte("\x1b]" + strings.Repeat("x", 10000)))); out != "" {
		t.Fatal("OSC exposed")
	}
	if out := string(p.feed([]byte("private tail\x1b\\safe"))); out != "safe" {
		t.Fatal(out)
	}
	s = newScreen(120, 240)
	for i := 0; i < 120; i++ {
		s.feed([]byte(fmt.Sprintf("\x1b[%d;1H", i+1) + strings.Repeat("😀", 240)))
	}
	session := &session{id: "screen", spec: Spec{PTY: true}, screen: s, done: make(chan struct{}), changed: make(chan struct{})}
	m := testManager(t, Hooks{})
	count := 0
	offset := 0
	for {
		out, err := m.read(session, 0, "screen", offset, "")
		r := result(t, out, err)
		if len(r.Screen.Lines) == 0 {
			t.Fatal("empty screen page")
		}
		count += len(r.Screen.Lines)
		if r.Screen.NextOffset < 0 {
			break
		}
		if r.Screen.NextOffset <= offset {
			t.Fatal("screen pagination stalled")
		}
		offset = r.Screen.NextOffset
	}
	if count != 120 {
		t.Fatal("screen rows omitted")
	}
}
func TestResizeThenCancellationDoesNotBlockPTYReader(t *testing.T) {
	m := testManager(t, Hooks{})
	r := start(t, m, "sleep 30", true)
	request(t, m, Request{Action: "resize", ID: r.ID, Rows: 40, Columns: 120})
	done := make(chan struct{})
	go func() { m.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("PTY read blocked shutdown after resize")
	}
}

func TestTaskCleanupIncludesSeparateJobGroups(t *testing.T) {
	dir := t.TempDir()
	m := New(context.Background(), HostLauncher(dir, []string{"PATH=/usr/bin:/bin"}), nil, Hooks{})
	defer m.Close()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	r := start(t, m, "Q_AGENT_GROUP_FIXTURE=1 "+shellQuote(exe)+" -test.run=^TestSessionGroupFixture$", true)
	until(t, m, r.ID, func(r Result) bool { return strings.Contains(r.Output, "separate-group-ready") })
	m.Close()
	time.Sleep(1100 * time.Millisecond)
	if _, err = os.Stat(filepath.Join(dir, "late")); !os.IsNotExist(err) {
		t.Fatal("job group survived session cleanup")
	}
}
func TestSessionGroupFixture(t *testing.T) {
	if os.Getenv("Q_AGENT_GROUP_FIXTURE") == "" {
		return
	}
	child := exec.Command("/bin/sh", "-c", "sleep 1; touch late")
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	fmt.Print("separate-group-ready")
	_ = child.Wait()
}
