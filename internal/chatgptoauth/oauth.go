// Package chatgptoauth implements the console-only ChatGPT OAuth login used by
// `q-agent login chatgpt`.
//
// Flow (manual paste-back, no localhost listener, no Telegram):
//
//	q-agent prints an authorize URL -> user opens it in a browser, approves ->
//	browser lands on http://localhost:1455/auth/callback?code=...&state=...
//	(which fails to connect; that is expected) -> user pastes the full address
//	bar URL back into the console -> q-agent exchanges code+PKCE verifier for
//	tokens and stores them with 0600 file rules.
//
// The client_id / endpoints mirror the Codex CLI PKCE flow.
package chatgptoauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

const (
	// ClientID mirrors the Codex CLI OAuth client.
	ClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	// AuthorizeEndpoint is the browser entry point.
	AuthorizeEndpoint = "https://auth.openai.com/oauth/authorize"
	// TokenEndpoint exchanges and refreshes tokens.
	TokenEndpoint = "https://auth.openai.com/oauth/token"
	// RedirectURI is only a rendezvous value: no listener is opened. The
	// browser redirect fails to connect and the user pastes the URL back.
	RedirectURI = "http://localhost:1455/auth/callback"
	// Scopes requests the standard OIDC set plus offline access for refresh.
	Scopes = "openid profile email offline_access"
	// DefaultPath is used when chatgpt.oauth_path is unset.
	DefaultPath = "~/.config/q-agent/chatgpt-oauth.json"
)

// refreshMargin triggers a refresh before the access token actually dies so a
// long model call does not race expiry.
const refreshMargin = 5 * time.Minute

// maxFileSize bounds the OAuth JSON file; tokens are short.
const maxFileSize = 64 * 1024

// Doer posts token HTTP requests. *http.Client satisfies it, as does the llm
// HTTPDoer (both use *net/http.Request/Response).
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Tokens is the persisted OAuth credential. ExpiresAt is unix seconds.
type Tokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    int64  `json:"expires_at"`
	AccountID    string `json:"account_id,omitempty"`
	Email        string `json:"email,omitempty"`
}

// Expired reports whether the access token is unusable.
func (t Tokens) Expired(now time.Time) bool {
	if t.AccessToken == "" {
		return true
	}
	return now.Unix() >= t.ExpiresAt
}

// NeedsRefresh reports whether the token should be refreshed proactively.
func (t Tokens) NeedsRefresh(now time.Time) bool {
	if t.AccessToken == "" || t.RefreshToken == "" {
		return true
	}
	return now.Add(refreshMargin).Unix() >= t.ExpiresAt
}

// NewVerifier generates a PKCE code_verifier (43-128 base64url chars).
func NewVerifier() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate PKCE verifier: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf[:]), nil
}

// Challenge derives the PKCE code_challenge for a verifier.
func Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// NewState generates a CSRF state value.
func NewState() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate OAuth state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf[:]), nil
}

// AuthorizeURL builds the browser URL for the given PKCE verifier and state.
func AuthorizeURL(verifier, state string) string {
	q := url.Values{}
	q.Set("client_id", ClientID)
	q.Set("redirect_uri", RedirectURI)
	q.Set("response_type", "code")
	q.Set("scope", Scopes)
	q.Set("code_challenge", Challenge(verifier))
	q.Set("code_challenge_method", "S256")
	q.Set("state", state)
	return AuthorizeEndpoint + "?" + q.Encode()
}

// ParseCallback extracts the authorization code from the pasted address-bar
// URL. wantState must match the state query value when non-empty.
func ParseCallback(pasted, wantState string) (string, error) {
	pasted = strings.TrimSpace(pasted)
	if pasted == "" {
		return "", fmt.Errorf("empty URL; paste the full address-bar URL after approving in the browser")
	}
	u, err := url.Parse(pasted)
	if err != nil {
		return "", fmt.Errorf("invalid URL: %w", err)
	}
	q := u.Query()
	if msg := q.Get("error"); msg != "" {
		desc := q.Get("error_description")
		if desc != "" {
			return "", fmt.Errorf("authorization failed: %s (%s)", msg, desc)
		}
		return "", fmt.Errorf("authorization failed: %s", msg)
	}
	code := q.Get("code")
	if code == "" {
		return "", fmt.Errorf("no code in URL; approve in the browser first, then paste the full http://localhost:1455/auth/callback?... URL")
	}
	if wantState != "" && q.Get("state") != wantState {
		return "", fmt.Errorf("state mismatch; the pasted URL is not from this login attempt, run the command again")
	}
	return code, nil
}

// ResolvePath expands ~ and returns an absolute path. Empty means default.
func ResolvePath(path string) (string, error) {
	if path == "" {
		path = DefaultPath
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if path == "~" {
			path = home
		} else {
			path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	return filepath.Abs(path)
}

func checkFile(stat os.FileInfo, path string) error {
	if stat.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("chatgpt oauth file %q must not be a symlink", path)
	}
	if !stat.Mode().IsRegular() {
		return fmt.Errorf("chatgpt oauth file %q must be a regular file", path)
	}
	if perm := stat.Mode().Perm(); perm != 0600 && perm != 0400 {
		return fmt.Errorf("chatgpt oauth file %q must have mode 0600 or 0400 (has %04o)", path, perm)
	}
	if runtime.GOOS == "linux" {
		native, ok := stat.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("chatgpt oauth file %q ownership unknown", path)
		}
		if native.Uid != uint32(os.Getuid()) {
			return fmt.Errorf("chatgpt oauth file %q must be owned by the current user", path)
		}
	}
	if stat.Size() > maxFileSize {
		return fmt.Errorf("chatgpt oauth file %q exceeds %d bytes", path, maxFileSize)
	}
	return nil
}

// Load reads and validates the stored tokens.
func Load(path string) (Tokens, error) {
	full, err := ResolvePath(path)
	if err != nil {
		return Tokens{}, err
	}
	link, err := os.Lstat(full)
	if err != nil {
		return Tokens{}, fmt.Errorf("chatgpt oauth file: %w; run `q-agent login chatgpt`", err)
	}
	if err := checkFile(link, full); err != nil {
		return Tokens{}, err
	}
	f, err := os.Open(full)
	if err != nil {
		return Tokens{}, fmt.Errorf("chatgpt oauth file: %w", err)
	}
	defer f.Close()
	live, err := f.Stat()
	if err != nil {
		return Tokens{}, fmt.Errorf("chatgpt oauth file: %w", err)
	}
	if err := checkFile(live, full); err != nil {
		return Tokens{}, err
	}
	data, err := io.ReadAll(io.LimitReader(f, maxFileSize+1))
	if err != nil {
		return Tokens{}, fmt.Errorf("chatgpt oauth file: %w", err)
	}
	var t Tokens
	if err := json.Unmarshal(data, &t); err != nil {
		return Tokens{}, fmt.Errorf("chatgpt oauth file is not valid JSON; run `q-agent login chatgpt` again")
	}
	if t.AccessToken == "" {
		return Tokens{}, fmt.Errorf("chatgpt oauth file has no access token; run `q-agent login chatgpt` again")
	}
	return t, nil
}

// Save atomically stores tokens with mode 0600.
func Save(path string, t Tokens) error {
	full, err := ResolvePath(path)
	if err != nil {
		return err
	}
	if link, err := os.Lstat(full); err == nil {
		if err := checkFile(link, full); err != nil {
			// Existing path violates strict rules (for example a symlink or
			// loose mode): refuse to follow/replace it blindly.
			return err
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("chatgpt oauth file: %w", err)
	}
	dir := filepath.Dir(full)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create oauth directory: %w", err)
	}
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".chatgpt-oauth-*")
	if err != nil {
		return fmt.Errorf("write oauth file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("write oauth file: %w", err)
	}
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return fmt.Errorf("write oauth file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write oauth file: %w", err)
	}
	if err := os.Rename(tmpName, full); err != nil {
		return fmt.Errorf("write oauth file: %w", err)
	}
	return nil
}

type tokenReply struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	AccountID    string `json:"account_id"`
	Email        string `json:"email"`
	IDToken      string `json:"id_token"`
}

func postForm(ctx context.Context, do Doer, form url.Values) (tokenReply, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenReply{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := do.Do(req)
	if err != nil {
		return tokenReply{}, fmt.Errorf("token endpoint: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return tokenReply{}, fmt.Errorf("token endpoint returned %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	var reply tokenReply
	if err := json.Unmarshal(data, &reply); err != nil {
		return tokenReply{}, fmt.Errorf("token endpoint returned invalid JSON")
	}
	if reply.AccessToken == "" {
		return tokenReply{}, fmt.Errorf("token endpoint returned no access token")
	}
	return reply, nil
}

// Exchange swaps an authorization code plus PKCE verifier for stored tokens.
func Exchange(ctx context.Context, do Doer, code, verifier string) (Tokens, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", ClientID)
	form.Set("redirect_uri", RedirectURI)
	form.Set("code", code)
	form.Set("code_verifier", verifier)
	reply, err := postForm(ctx, do, form)
	if err != nil {
		return Tokens{}, err
	}
	expires := time.Now().Add(time.Duration(reply.ExpiresIn) * time.Second)
	if reply.ExpiresIn <= 0 {
		expires = time.Now().Add(time.Hour)
	}
	return Tokens{
		AccessToken:  reply.AccessToken,
		RefreshToken: reply.RefreshToken,
		ExpiresAt:    expires.Unix(),
		AccountID:    reply.AccountID,
		Email:        reply.Email,
	}, nil
}

// Refresh swaps a refresh token for new tokens.
func Refresh(ctx context.Context, do Doer, refreshToken string) (Tokens, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("client_id", ClientID)
	form.Set("refresh_token", refreshToken)
	reply, err := postForm(ctx, do, form)
	if err != nil {
		return Tokens{}, err
	}
	expires := time.Now().Add(time.Duration(reply.ExpiresIn) * time.Second)
	if reply.ExpiresIn <= 0 {
		expires = time.Now().Add(time.Hour)
	}
	if reply.RefreshToken == "" {
		reply.RefreshToken = refreshToken
	}
	return Tokens{
		AccessToken:  reply.AccessToken,
		RefreshToken: reply.RefreshToken,
		ExpiresAt:    expires.Unix(),
		AccountID:    reply.AccountID,
		Email:        reply.Email,
	}, nil
}

// GetValidToken returns a usable access token, refreshing and persisting when
// the stored token is near expiry.
func GetValidToken(ctx context.Context, do Doer, path string) (string, error) {
	t, err := Load(path)
	if err != nil {
		return "", err
	}
	if !t.NeedsRefresh(time.Now()) {
		return t.AccessToken, nil
	}
	if t.RefreshToken == "" {
		return "", fmt.Errorf("chatgpt token expired and no refresh token; run `q-agent login chatgpt` again")
	}
	next, err := Refresh(ctx, do, t.RefreshToken)
	if err != nil {
		return "", fmt.Errorf("chatgpt token refresh failed; run `q-agent login chatgpt` again: %w", err)
	}
	if next.AccountID == "" {
		next.AccountID = t.AccountID
	}
	if next.Email == "" {
		next.Email = t.Email
	}
	if err := Save(path, next); err != nil {
		return "", err
	}
	return next.AccessToken, nil
}

// RefreshAndSave forces a refresh regardless of expiry (used for a 401 retry).
func RefreshAndSave(ctx context.Context, do Doer, path string) (string, error) {
	t, err := Load(path)
	if err != nil {
		return "", err
	}
	if t.RefreshToken == "" {
		return "", fmt.Errorf("no refresh token; run `q-agent login chatgpt` again")
	}
	next, err := Refresh(ctx, do, t.RefreshToken)
	if err != nil {
		return "", fmt.Errorf("chatgpt token refresh failed; run `q-agent login chatgpt` again: %w", err)
	}
	if next.AccountID == "" {
		next.AccountID = t.AccountID
	}
	if next.Email == "" {
		next.Email = t.Email
	}
	if err := Save(path, next); err != nil {
		return "", err
	}
	return next.AccessToken, nil
}
