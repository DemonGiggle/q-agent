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
	"syscall"
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

// siwcTokens builds a current-format credential for tests.
func siwcTokens(access, refresh string, expiry time.Time) Tokens {
	saved := expiry.Add(-time.Hour)
	if access == "fresh-access" || access == "test-access" || access == "a" {
		saved = time.Now()
		expiry = saved.Add(time.Hour)
	}
	return Tokens{
		Email:          "user@example.com",
		Issuer:         Issuer,
		Subject:        "sub-123",
		ClientID:       "oaiapp_test123",
		ExtAgentHostID: "urn:uuid:host-1",
		IDToken:        "id-token",
		AccessToken:    access,
		RefreshToken:   refresh,
		TokenType:      "Bearer",
		ExpiresIn:      3600,
		Scopes:         []string{"openid", "profile", "email", "offline_access", "resource.invoke", PlanScope},
		SavedAt:        saved.UTC().Format(time.RFC3339),
		ExpiresAt:      expiry.Unix(),
		AccountID:      "sub-123",
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

func TestExchangeRequiresIssuedClient(t *testing.T) {
	do := stubDoer(func(r *http.Request) (*http.Response, error) {
		t.Error("token endpoint must not be called")
		return &http.Response{StatusCode: 500, Status: "500", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	if _, _, err := Exchange(context.Background(), do, "code", "verifier", DynamicClientID, CallbackURL(1455)); err == nil {
		t.Fatal("dynamic client accepted for exchange")
	}
	if _, _, err := Exchange(context.Background(), do, "code", "verifier", "", CallbackURL(1455)); err == nil {
		t.Fatal("empty client accepted for exchange")
	}
}

func TestExchangeSendsResourceAndClient(t *testing.T) {
	access := jwtWithAccount("jwt-acct")
	var gotForm string
	var gotClient, gotResource, gotURI string
	do := stubDoer(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		gotForm = string(raw)
		_ = r.ParseForm()
		// Parse manually from body since stub doesn't populate Form.
		gotClient = formValue(gotForm, "client_id")
		gotResource = formValue(gotForm, "resource")
		gotURI = formValue(gotForm, "redirect_uri")
		body := `{"access_token":"` + access + `","refresh_token":"r1","expires_in":3600,"scope":"openid profile email offline_access resource.invoke chatgpt.tokens.use.direct","id_token":"id1"}`
		return &http.Response{StatusCode: 200, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	tok, _, err := Exchange(context.Background(), do, "code", "verifier", "oaiapp_abc", CallbackURL(1455))
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccountID != "jwt-acct" {
		t.Fatalf("account = %q", tok.AccountID)
	}
	if gotClient != "oaiapp_abc" || gotResource != Resource || !strings.Contains(gotURI, "127.0.0.1") {
		t.Fatalf("form client=%q resource=%q uri=%q", gotClient, gotResource, gotURI)
	}
}

func formValue(form, key string) string {
	for _, part := range strings.Split(form, "&") {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) == 2 && kv[0] == key {
			v, _ := urlUnescape(kv[1])
			return v
		}
	}
	return ""
}

func urlUnescape(s string) (string, error) {
	// Minimal unescape for test assertions (handles : / . @).
	r := strings.ReplaceAll(s, "%3A", ":")
	r = strings.ReplaceAll(r, "%2F", "/")
	r = strings.ReplaceAll(r, "%40", "@")
	r = strings.ReplaceAll(r, "+", " ")
	return r, nil
}

func TestGetValidTokensSerializesConcurrentRefresh(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "oauth.json")
	expired := siwcTokens("old-access", "shared-refresh", time.Now().Add(-time.Hour))
	writeTokens(t, path, expired)
	var calls atomic.Int32
	do := stubDoer(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		// Simulate single-use rotating refresh: small delay to force overlap.
		time.Sleep(50 * time.Millisecond)
		body := `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600,"scope":"openid profile email offline_access resource.invoke chatgpt.tokens.use.direct","id_token":"new-id"}`
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
	if toks[0].ClientID != "oaiapp_test123" || toks[0].Subject != "sub-123" {
		t.Fatalf("identity not preserved: %+v", toks[0])
	}
}

func TestLegacyRequiresRelogin(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "oauth.json")
	legacy := Tokens{AccessToken: "old-access", RefreshToken: "r", ExpiresAt: time.Now().Add(time.Hour).Unix(), AccountID: "acct-1"}
	writeTokens(t, path, legacy)
	do := stubDoer(func(r *http.Request) (*http.Response, error) {
		t.Error("token endpoint must not be called for legacy")
		return &http.Response{StatusCode: 500, Status: "500", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	if _, err := GetValidTokens(context.Background(), do, path); err == nil || !strings.Contains(err.Error(), "login chatgpt") {
		t.Fatalf("err = %v, want login prompt", err)
	}
}

func TestEnsureValidFailsFastWithoutRefresh(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "oauth.json")
	expired := siwcTokens("old-access", "", time.Now().Add(-time.Hour))
	expired.RefreshToken = ""
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
	fresh := siwcTokens("fresh-access", "fresh-refresh", time.Now().Add(time.Hour))
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

func TestGetValidTokensFastPathSkipsFileLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "oauth.json")
	fresh := siwcTokens("fresh-access", "fresh-refresh", time.Now().Add(time.Hour))
	writeTokens(t, path, fresh)
	full, err := ResolvePath(path)
	if err != nil {
		t.Fatal(err)
	}
	// Hold the cross-process lock: fresh credentials must return via the
	// lock-free fast path without blocking.
	lockFile, err := os.OpenFile(full+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		t.Skipf("flock unavailable: %v", err)
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
	do := stubDoer(func(r *http.Request) (*http.Response, error) {
		t.Error("token endpoint must not be called on fresh fast path")
		return &http.Response{StatusCode: 500, Status: "500", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	got, err := GetValidTokens(ctx, do, path)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "fresh-access" {
		t.Fatalf("got %+v", got)
	}
	if elapsed > time.Second {
		t.Fatalf("fast path blocked on file lock for %v", elapsed)
	}
}

func TestGetValidTokensStaleRespectsContextOnLockedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "oauth.json")
	stale := siwcTokens("stale-access", "stale-refresh", time.Now().Add(-time.Hour))
	writeTokens(t, path, stale)
	full, err := ResolvePath(path)
	if err != nil {
		t.Fatal(err)
	}
	lockFile, err := os.OpenFile(full+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		t.Skipf("flock unavailable: %v", err)
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
	called := false
	do := stubDoer(func(r *http.Request) (*http.Response, error) {
		called = true
		return &http.Response{StatusCode: 200, Status: "200", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = GetValidTokens(ctx, do, path)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected context error while file lock held, got success")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("did not respect context deadline, blocked %v", elapsed)
	}
	if called {
		t.Fatal("token endpoint must not be called while blocked on file lock")
	}
}

func TestRefreshStaleFastPathSkipsLockedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "oauth.json")
	fresh := siwcTokens("fresh-access", "fresh-refresh", time.Now().Add(time.Hour))
	writeTokens(t, path, fresh)
	full, err := ResolvePath(path)
	if err != nil {
		t.Fatal(err)
	}
	lockFile, err := os.OpenFile(full+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		t.Skipf("flock unavailable: %v", err)
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
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
