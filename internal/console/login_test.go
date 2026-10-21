package console

import (
	"bytes"
	"strings"
	"testing"
)

func TestLoginRequiresTerminal(t *testing.T) {
	path := consoleConfig(t)
	var out bytes.Buffer
	_, err := run([]string{"login", "chatgpt", "--config", path}, noRead{t}, &out, &out, false)
	if err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("expected terminal error, got %v", err)
	}
}

func TestLoginRejectsUnknownTarget(t *testing.T) {
	path := consoleConfig(t)
	var out bytes.Buffer
	handled, err := run([]string{"login", "github", "--config", path}, strings.NewReader(""), &out, &out, true)
	if !handled || err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
}

func TestLoginRejectsJSON(t *testing.T) {
	path := consoleConfig(t)
	var out bytes.Buffer
	_, err := run([]string{"login", "chatgpt", "--json", "--config", path}, strings.NewReader(""), &out, &out, true)
	if err == nil || !strings.Contains(err.Error(), "--json") {
		t.Fatalf("expected --json error, got %v", err)
	}
}
