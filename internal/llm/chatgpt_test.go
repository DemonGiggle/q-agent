package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeChatGPTOAuth(t *testing.T, access, refresh, account string, expires time.Time) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "oauth.json")
	data, _ := json.Marshal(map[string]any{
		"access_token":  access,
		"refresh_token": refresh,
		"expires_at":    expires.Unix(),
		"account_id":    account,
	})
	if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestChatGPTCodexRequestContract(t *testing.T) {
	oauthPath := writeChatGPTOAuth(t, "test-access", "test-refresh", "acct-1", time.Now().Add(time.Hour))
	var gotBody map[string]json.RawMessage
	var gotAccount, gotAuth string
	var gotPath string
	client := doerFunc(func(request *http.Request) (*http.Response, error) {
		gotPath = request.URL.Path
		gotAuth = request.Header.Get("Authorization")
		gotAccount = request.Header.Get("ChatGPT-Account-Id")
		body, _ := io.ReadAll(request.Body)
		if err := json.Unmarshal(body, &gotBody); err != nil {
			t.Fatalf("body not JSON: %v", err)
		}
		if err := json.Unmarshal(gotBody["store"], new(bool)); err != nil {
			t.Fatalf("store missing: %s", body)
		}
		var store bool
		_ = json.Unmarshal(gotBody["store"], &store)
		if store {
			t.Errorf("store must be false for Codex, body=%s", body)
		}
		if _, ok := gotBody["instructions"]; !ok {
			t.Errorf("instructions missing, body=%s", body)
		} else {
			var instr string
			_ = json.Unmarshal(gotBody["instructions"], &instr)
			if !strings.Contains(instr, "be helpful") {
				t.Errorf("instructions = %q", instr)
			}
		}
		// System must not appear inside input.
		if strings.Contains(string(gotBody["input"]), "be helpful") {
			t.Errorf("system leaked into input: %s", gotBody["input"])
		}
		stream := `data: {"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1},"output":[{"type":"message","content":[{"type":"output_text","text":"hi"}]}]}}` + "\n\n"
		return &http.Response{StatusCode: 200, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(stream))}, nil
	})
	p, err := newChatGPT(Config{ChatGPTOAuthPath: oauthPath, HTTP: client})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Complete(context.Background(), Request{
		Model:    "gpt-5.6-codex",
		Messages: []Message{{Role: "system", Content: "be helpful"}, {Role: "user", Content: "hello"}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(gotPath, "/responses") {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuth != "Bearer test-access" {
		t.Fatalf("auth = %q", gotAuth)
	}
	if gotAccount != "acct-1" {
		t.Fatalf("account header = %q", gotAccount)
	}
}

func TestChatGPT401RefreshesOnceWithStaleReuse(t *testing.T) {
	oauthPath := writeChatGPTOAuth(t, "stale-access", "r1", "acct-1", time.Now().Add(time.Hour))
	calls := 0
	client := doerFunc(func(request *http.Request) (*http.Response, error) {
		// Token endpoint.
		if strings.Contains(request.URL.String(), "auth.openai.com") {
			calls++
			body := `{"access_token":"fresh-access","refresh_token":"r2","expires_in":3600,"account_id":"acct-1"}`
			return &http.Response{StatusCode: 200, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
		}
		if request.Header.Get("Authorization") == "Bearer stale-access" {
			body := `provider returned 401 Unauthorized: expired`
			return &http.Response{StatusCode: 401, Status: "401 Unauthorized", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
		}
		if request.Header.Get("Authorization") != "Bearer fresh-access" {
			t.Fatalf("second auth = %q", request.Header.Get("Authorization"))
		}
		stream := `data: {"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1},"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}}` + "\n\n"
		return &http.Response{StatusCode: 200, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(stream))}, nil
	})
	// Route the OAuth Doer through the same stub: chatgptoauth uses the llm
	// HTTPDoer adapter which is *http.Request/Response compatible.
	p, err := newChatGPT(Config{ChatGPTOAuthPath: oauthPath, HTTP: client})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := p.Complete(context.Background(), Request{Model: "gpt-5.6-codex", Messages: []Message{{Role: "user", Content: "hi"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message.Content != "ok" {
		t.Fatalf("resp = %+v", resp)
	}
	if calls != 1 {
		t.Fatalf("refresh calls = %d, want 1", calls)
	}
}
