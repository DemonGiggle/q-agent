package chatgptoauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type stubDoer func(*http.Request) (*http.Response, error)

func (f stubDoer) Do(r *http.Request) (*http.Response, error) { return f(r) }

func writeTokens(t *testing.T, path string, tok Tokens) {
	t.Helper()
	full, err := ResolvePath(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(tok)
	if err := os.WriteFile(full, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
}

func jwtWithAccount(account string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256"}`))
	payload, _ := json.Marshal(map[string]string{"account_id": account})
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func TestAccountIDFromAccessToken(t *testing.T) {
	if got := AccountIDFromAccessToken(jwtWithAccount("acct-123")); got != "acct-123" {
		t.Fatalf("got %q", got)
	}
	if got := AccountIDFromAccessToken("opaque"); got != "" {
		t.Fatalf("opaque should be empty, got %q", got)
	}
	// Nested claim.
	header := base64.RawURLEncoding.EncodeToString([]byte(`{}`))
	inner, _ := json.Marshal(map[string]any{"https://api.openai.com/auth": map[string]any{"account_id": "nested-1"}})
	tok := header + "." + base64.RawURLEncoding.EncodeToString(inner) + ".sig"
	if got := AccountIDFromAccessToken(tok); got != "nested-1" {
		t.Fatalf("nested got %q", got)
	}
}

func TestExchangeFillsAccountFromJWT(t *testing.T) {
	access := jwtWithAccount("jwt-acct")
	do := stubDoer(func(r *http.Request) (*http.Response, error) {
		body := `{"access_token":"` + access + `","refresh_token":"r1","expires_in":3600}`
		return &http.Response{StatusCode: 200, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	tok, err := Exchange(context.Background(), do, "code", "verifier")
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccountID != "jwt-acct" {
		t.Fatalf("account = %q", tok.AccountID)
	}
}

func TestGetValidTokensSerializesConcurrentRefresh(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "oauth.json")
	expired := Tokens{AccessToken: "old-access", RefreshToken: "shared-refresh", ExpiresAt: time.Now().Add(-time.Hour).Unix()}
	writeTokens(t, path, expired)
	var calls atomic.Int32
	do := stubDoer(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		// Simulate single-use rotating refresh: small delay to force overlap.
		time.Sleep(50 * time.Millisecond)
		body := `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600,"account_id":"a1"}`
		return &http.Response{StatusCode: 200, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	var wg sync.WaitGroup
	errs := make([]error, 2)
	toks := make([]Tokens, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tok, err := GetValidTokens(context.Background(), do, path)
			toks[i] = tok
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("refresh err: %v", err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("concurrent refresh sent %d token calls, want 1", calls.Load())
	}
	if toks[0].AccessToken != "new-access" || toks[1].AccessToken != "new-access" {
		t.Fatalf("tokens = %+v", toks)
	}
}

func TestEnsureValidFailsFastWithoutRefresh(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "oauth.json")
	expired := Tokens{AccessToken: "old-access", ExpiresAt: time.Now().Add(-time.Hour).Unix()}
	writeTokens(t, path, expired)
	do := stubDoer(func(r *http.Request) (*http.Response, error) {
		t.Error("token endpoint must not be called without refresh token")
		return &http.Response{StatusCode: 500, Status: "500", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	if _, err := EnsureValid(context.Background(), do, path); err == nil {
		t.Fatal("expected login error")
	} else if !strings.Contains(err.Error(), "q-agent login chatgpt") {
		t.Fatalf("err = %v", err)
	}
}

func TestRefreshAndSaveWithStaleReusesFresh(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "oauth.json")
	fresh := Tokens{AccessToken: "fresh-access", RefreshToken: "fresh-refresh", ExpiresAt: time.Now().Add(time.Hour).Unix(), AccountID: "a9"}
	writeTokens(t, path, fresh)
	called := false
	do := stubDoer(func(r *http.Request) (*http.Response, error) {
		called = true
		return &http.Response{StatusCode: 200, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})
	got, err := RefreshAndSaveWithStale(context.Background(), do, path, "stale-access")
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "fresh-access" {
		t.Fatalf("should reuse fresh, got %+v", got)
	}
	if called {
		t.Fatal("should not call token endpoint when fresh already")
	}
}
