// Package chatgptoauth implements the Sign in with ChatGPT login used by
// `q-agent login chatgpt`.
//
// Flow follows the open-source token-sharing sign-in documented at
// https://developers.openai.com/siwc/token-sharing-open-source/sign-in :
//
//	q-agent starts a 127.0.0.1 loopback listener -> opens the browser at
//	https://auth.openai.com/api/accounts/authorize -> browser returns to the
//	loopback callback with code + issued client_id -> q-agent exchanges the
//	code (with the same redirect_uri and resource) for tokens, validates the
//	ID token, checks granted chatgpt.tokens.use.direct, and stores the
//	credential record with 0600 file rules.
//
// First-time registration uses client_id=dynamic_agent_client with
// agent_name_hint and ext_agent_host_id. The callback returns the issued
// client_id (oaiapp_...), which is saved and reused for later sign-ins.
// dynamic_agent_client is never saved or used for token exchange.
package chatgptoauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"q-agent/internal/redaction"
)

const (
	// DynamicClientID is the first-time registration entrypoint. It is never
	// saved as the connection's issued ID and never used for token exchange.
	DynamicClientID = "dynamic_agent_client"
	// AgentNameHint is this app's actual name, sent only on initial dynamic
	// registration. The user can rename the agent before approval; it is
	// display metadata, not identity.
	AgentNameHint = "q-agent"
	// AuthorizeEndpoint is the browser entry point.
	AuthorizeEndpoint = "https://auth.openai.com/api/accounts/authorize"
	// TokenEndpoint exchanges and refreshes tokens.
	TokenEndpoint = "https://auth.openai.com/api/accounts/oauth/token"
	// Issuer is the expected ID-token issuer.
	Issuer = "https://auth.openai.com"
	// JWKSURI publishes the ID-token signing keys.
	JWKSURI = "https://auth.openai.com/.well-known/jwks.json"
	// OpenIDConfiguration advertises issuer, endpoints, and revocation.
	OpenIDConfiguration = "https://auth.openai.com/.well-known/openid-configuration"
	// Resource is the OAuth resource for ChatGPT plan usage.
	Resource = "https://api.openai.com/v1"
	// PlanScope gates ChatGPT plan usage. A valid ID token alone does not
	// authorize inference.
	PlanScope = "chatgpt.tokens.use.direct"
	// Scopes is the complete requested scope set: identity plus plan usage.
	Scopes = "openid profile email offline_access resource.invoke chatgpt.tokens.use.direct"
	// CallbackPath is the only path that may be used; only the port varies.
	CallbackPath = "/auth/callback"
	// PreferredCallbackPort is tried first for the loopback listener.
	PreferredCallbackPort = 1455
	// DefaultPath is used when chatgpt.oauth_path is unset.
	DefaultPath = "~/.config/q-agent/chatgpt-oauth.json"
	// DefaultHostPath persists this host's ext_agent_host_id.
	DefaultHostPath = "~/.config/q-agent/chatgpt-host-id"
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

// Tokens is the persisted SIWC credential record. ExpiresAt and AccountID are
// retained for existing callers: ExpiresAt mirrors SavedAt+ExpiresIn, and
// AccountID mirrors Subject (validated ID-token sub).
type Tokens struct {
	Email          string   `json:"email,omitempty"`
	Issuer         string   `json:"issuer,omitempty"`
	Subject        string   `json:"subject,omitempty"`
	ClientID       string   `json:"client_id,omitempty"`
	ExtAgentHostID string   `json:"ext_agent_host_id,omitempty"`
	IDToken        string   `json:"id_token,omitempty"`
	AccessToken    string   `json:"access_token"`
	RefreshToken   string   `json:"refresh_token,omitempty"`
	TokenType      string   `json:"token_type,omitempty"`
	ExpiresIn      int64    `json:"expires_in,omitempty"`
	Scopes         []string `json:"scopes,omitempty"`
	SavedAt        string   `json:"saved_at,omitempty"`
	ExpiresAt      int64    `json:"expires_at,omitempty"`
	AccountID      string   `json:"account_id,omitempty"`
}

// Expiry returns when the access token dies.
func (t Tokens) Expiry() time.Time {
	if t.SavedAt != "" && t.ExpiresIn > 0 {
		if saved, err := time.Parse(time.RFC3339, t.SavedAt); err == nil {
			return saved.Add(time.Duration(t.ExpiresIn) * time.Second)
		}
	}
	if t.ExpiresAt != 0 {
		return time.Unix(t.ExpiresAt, 0)
	}
	return time.Time{}
}

// Expired reports whether the access token is unusable.
func (t Tokens) Expired(now time.Time) bool {
	if t.AccessToken == "" {
		return true
	}
	exp := t.Expiry()
	if exp.IsZero() {
		return true
	}
	return !now.Before(exp)
}

// NeedsRefresh reports whether the token should be refreshed proactively.
func (t Tokens) NeedsRefresh(now time.Time) bool {
	if t.AccessToken == "" || t.RefreshToken == "" {
		return true
	}
	exp := t.Expiry()
	if exp.IsZero() {
		return true
	}
	return !now.Add(refreshMargin).Before(exp)
}

// HasPlanScope reports whether the granted scopes authorize ChatGPT plan use.
func (t Tokens) HasPlanScope() bool {
	for _, s := range t.Scopes {
		if s == PlanScope {
			return true
		}
	}
	return false
}

// Identity returns the validated account identity (sub), falling back to the
// legacy account field.
func (t Tokens) Identity() string {
	if t.Subject != "" {
		return t.Subject
	}
	return t.AccountID
}

// IsLegacy reports a pre-SIWC credential without an issued client record.
// Legacy files predate dynamic_agent_client registration, ID-token hints, and
// resource-bound plan scopes and must re-login.
func (t Tokens) IsLegacy() bool {
	return t.ClientID == "" || t.IDToken == "" || !strings.HasPrefix(t.ClientID, "oaiapp_")
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

// NewNonce generates an OIDC nonce for ID-token validation.
func NewNonce() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate OIDC nonce: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf[:]), nil
}

// AuthorizeParams builds one authorization attempt. ClientID is either
// DynamicClientID (new account) or the saved issued client ID (returning
// account). AgentNameHint is included only for new registrations.
// IDTokenHint/LoginHint are sent only for returning accounts.
type AuthorizeParams struct {
	ClientID     string
	RedirectURI  string
	State        string
	Nonce        string
	Verifier     string
	HostID       string
	IDTokenHint  string
	LoginHint    string
	IsNewAccount bool
}

// AuthorizeURL builds the browser URL for one attempt.
func AuthorizeURL(p AuthorizeParams) string {
	q := url.Values{}
	q.Set("client_id", p.ClientID)
	if p.IsNewAccount {
		q.Set("agent_name_hint", AgentNameHint)
	}
	if p.HostID != "" {
		q.Set("ext_agent_host_id", p.HostID)
	}
	if !p.IsNewAccount {
		if p.IDTokenHint != "" {
			q.Set("id_token_hint", p.IDTokenHint)
		}
		if p.LoginHint != "" {
			q.Set("login_hint", p.LoginHint)
		}
	}
	q.Set("response_type", "code")
	q.Set("redirect_uri", p.RedirectURI)
	q.Set("scope", Scopes)
	q.Set("resource", Resource)
	q.Set("state", p.State)
	q.Set("nonce", p.Nonce)
	q.Set("code_challenge", Challenge(p.Verifier))
	q.Set("code_challenge_method", "S256")
	return AuthorizeEndpoint + "?" + q.Encode()
}

// CallbackURL returns the loopback callback for a port. Only the port varies.
func CallbackURL(port int) string {
	return fmt.Sprintf("http://127.0.0.1:%d%s", port, CallbackPath)
}

// CallbackResult is the validated loopback/paste-back result.
type CallbackResult struct {
	Code     string
	ClientID string
	Scope    string
	Scopes   []string
}

// ParseCallback extracts the authorization code from the pasted address-bar
// URL. wantState must match the state query value when non-empty. For new
// registrations the callback supplies the issued client_id; on reauthorization
// it may omit client_id, in which case pendingClientID (the ID sent with this
// attempt) is retained. A different supplied client ID is rejected.
func ParseCallback(pasted, wantState, pendingClientID string, isNewAccount bool) (CallbackResult, error) {
	pasted = strings.TrimSpace(pasted)
	if pasted == "" {
		return CallbackResult{}, fmt.Errorf("empty URL; paste the full address-bar URL after approving in the browser")
	}
	u, err := url.Parse(pasted)
	if err != nil {
		return CallbackResult{}, fmt.Errorf("invalid URL: %w", err)
	}
	q := u.Query()
	if msg := q.Get("error"); msg != "" {
		desc := q.Get("error_description")
		if desc != "" {
			return CallbackResult{}, fmt.Errorf("authorization failed: %s (%s)", msg, desc)
		}
		return CallbackResult{}, fmt.Errorf("authorization failed: %s", msg)
	}
	code := q.Get("code")
	if code == "" {
		return CallbackResult{}, fmt.Errorf("no code in URL; approve in the browser first, then paste the full %s?... URL", CallbackURL(PreferredCallbackPort))
	}
	if wantState != "" && q.Get("state") != wantState {
		return CallbackResult{}, fmt.Errorf("state mismatch; the pasted URL is not from this login attempt, run the command again")
	}
	gotClient := q.Get("client_id")
	scopeRaw := q.Get("scope")
	if isNewAccount {
		if gotClient == "" {
			return CallbackResult{}, fmt.Errorf("registration incomplete: callback did not include an issued client_id")
		}
		if gotClient == DynamicClientID {
			return CallbackResult{}, fmt.Errorf("registration incomplete: save the issued client_id, not dynamic_agent_client")
		}
	} else {
		if gotClient == "" {
			gotClient = pendingClientID
		} else if pendingClientID != "" && gotClient != pendingClientID {
			return CallbackResult{}, fmt.Errorf("client mismatch: callback client_id does not match this account's registration")
		}
	}
	return CallbackResult{Code: code, ClientID: gotClient, Scope: scopeRaw, Scopes: splitScope(scopeRaw)}, nil
}

func splitScope(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	return strings.Fields(raw)
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

// ResolveHostPath expands ~ for the host-ID file. Empty means default.
func ResolveHostPath(path string) (string, error) {
	if path == "" {
		path = DefaultHostPath
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

// EnsureHostID loads this host's ext_agent_host_id or creates and persists
// one (urn:uuid:...). The same host must send the same value on every sign-in.
func EnsureHostID(path string) (string, error) {
	full, err := ResolveHostPath(path)
	if err != nil {
		return "", err
	}
	if data, err := os.ReadFile(full); err == nil {
		id := strings.TrimSpace(string(data))
		if validHostID(id) {
			return id, nil
		}
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("host id file: %w", err)
	}
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate host id: %w", err)
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	id := fmt.Sprintf("urn:uuid:%08x-%04x-%04x-%04x-%012x",
		buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16])
	dir := filepath.Dir(full)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("create host id directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".chatgpt-host-id-*")
	if err != nil {
		return "", fmt.Errorf("write host id file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.WriteString(id + "\n"); err != nil {
		tmp.Close()
		return "", fmt.Errorf("write host id file: %w", err)
	}
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return "", fmt.Errorf("write host id file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("write host id file: %w", err)
	}
	if err := os.Rename(tmpName, full); err != nil {
		return "", fmt.Errorf("write host id file: %w", err)
	}
	return id, nil
}

func validHostID(id string) bool {
	if id == "" || len(id) > 256 {
		return false
	}
	if strings.ContainsAny(id, " \t\r\n") {
		return false
	}
	return true
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

// legacyTokens reads the pre-SIWQ file shape for migration detection.
type legacyTokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    int64  `json:"expires_at"`
	AccountID    string `json:"account_id,omitempty"`
	Email        string `json:"email,omitempty"`
}

// Load reads and validates the stored tokens, accepting the SIWC record and
// the legacy shape (legacy callers keep working until re-login).
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
	// Backfill derived fields.
	if t.AccountID == "" {
		t.AccountID = t.Subject
	}
	if t.Subject == "" {
		t.Subject = t.AccountID
	}
	if t.ExpiresAt == 0 && t.SavedAt != "" && t.ExpiresIn > 0 {
		if saved, err := time.Parse(time.RFC3339, t.SavedAt); err == nil {
			t.ExpiresAt = saved.Add(time.Duration(t.ExpiresIn) * time.Second).Unix()
		}
	}
	if t.SavedAt == "" && t.ExpiresAt != 0 {
		// Legacy record: keep ExpiresAt as the source of truth.
		var legacy legacyTokens
		_ = json.Unmarshal(data, &legacy)
		_ = legacy
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
	// Keep legacy mirrors in sync so existing readers/caches keep working.
	if t.AccountID == "" {
		t.AccountID = t.Subject
	}
	if t.Subject == "" {
		t.Subject = t.AccountID
	}
	if t.SavedAt != "" && t.ExpiresIn > 0 {
		if saved, err := time.Parse(time.RFC3339, t.SavedAt); err == nil {
			t.ExpiresAt = saved.Add(time.Duration(t.ExpiresIn) * time.Second).Unix()
		}
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
	redactTokens(t)
	return nil
}

// ClearTokens removes the stored credential file (sign-out). The host ID is
// retained for the next sign-in. Missing files are not an error.
func ClearTokens(path string) error {
	full, err := ResolvePath(path)
	if err != nil {
		return err
	}
	if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove oauth file: %w", err)
	}
	_ = os.Remove(full + ".lock")
	return nil
}

// redactTokens registers current secrets in the process-wide redaction
// registry so Policies created before a refresh (Runtime, Store, live
// output) still mask the rotated values on every sink.
func redactTokens(t Tokens) {
	if t.AccessToken != "" || t.RefreshToken != "" || t.IDToken != "" {
		redaction.AddSecrets(t.AccessToken, t.RefreshToken, t.IDToken)
	}
}

type tokenReply struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
	IDToken      string `json:"id_token"`
	AccountID    string `json:"account_id"`
	Email        string `json:"email"`
}

// ErrInvalidGrant marks a stale/rotated code or refresh token. Callers should
// discard the code and start a fresh authorization with the retained issued
// client ID.
type ErrInvalidGrant struct{ msg string }

func (e *ErrInvalidGrant) Error() string { return e.msg }

func isInvalidGrantMessage(msg string) bool {
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "invalid_grant")
}

func postForm(ctx context.Context, do Doer, form url.Values) (tokenReply, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenReply{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := do.Do(req)
	if err != nil {
		return tokenReply{}, fmt.Errorf("token endpoint: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := fmt.Sprintf("token endpoint returned %s: %s", resp.Status, strings.TrimSpace(string(data)))
		if isInvalidGrantMessage(string(data)) {
			return tokenReply{}, &ErrInvalidGrant{msg: msg}
		}
		return tokenReply{}, fmt.Errorf("%s", msg)
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

// AccountIDFromAccessToken extracts the ChatGPT account identifier from the
// access-token JWT claims. The token endpoint sometimes omits the top-level
// account_id; the JWT itself may carry it. Returns "" when the token
// is opaque or carries no recognized claim. Retained for legacy files; new
// SIWC identity is the validated ID-token sub plus issued client_id.
func AccountIDFromAccessToken(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	payload := parts[1]
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		// Lenient fallback: padded encoding.
		raw, err = base64.URLEncoding.DecodeString(payload)
		if err != nil {
			return ""
		}
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		return ""
	}
	for _, key := range []string{"chatgpt_account_id", "account_id", "accountId", "chatgpt-account-id"} {
		if v, ok := claims[key]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	// Some issuers nest the account under an auth object or a namespaced
	// claim URI. Search one level deep for the same keys.
	for _, v := range claims {
		obj, ok := v.(map[string]any)
		if !ok {
			continue
		}
		for _, key := range []string{"chatgpt_account_id", "account_id", "accountId"} {
			if s, ok := obj[key].(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

// accountIDFor prefers the token endpoint's top-level account_id, then falls
// back to the access-token (and id-token) JWT claims, then to the previously
// stored value so rotations never blank a known account.
func accountIDFor(reply tokenReply, fallback string) string {
	if reply.AccountID != "" {
		return reply.AccountID
	}
	if id := AccountIDFromAccessToken(reply.AccessToken); id != "" {
		return id
	}
	if reply.IDToken != "" {
		if id := AccountIDFromAccessToken(reply.IDToken); id != "" {
			return id
		}
	}
	return fallback
}

// tokensFromReply builds the SIWC credential record. savedAt is when the app
// received the token response; granted scopes come from the space-separated
// scope value. subject/email/issuer come from the validated ID token.
func tokensFromReply(reply tokenReply, issuedClientID, hostID, subject, email, issuer string, savedAt time.Time) Tokens {
	if savedAt.IsZero() {
		savedAt = time.Now()
	}
	expires := reply.ExpiresIn
	if expires <= 0 {
		expires = 3600
	}
	tokenType := reply.TokenType
	if tokenType == "" {
		tokenType = "Bearer"
	}
	scopes := splitScope(reply.Scope)
	savedUTC := savedAt.UTC().Format(time.RFC3339)
	account := accountIDFor(reply, subject)
	return Tokens{
		Email:          email,
		Issuer:         issuer,
		Subject:        subject,
		ClientID:       issuedClientID,
		ExtAgentHostID: hostID,
		IDToken:        reply.IDToken,
		AccessToken:    reply.AccessToken,
		RefreshToken:   reply.RefreshToken,
		TokenType:      tokenType,
		ExpiresIn:      expires,
		Scopes:         scopes,
		SavedAt:        savedUTC,
		ExpiresAt:      savedAt.Add(time.Duration(expires) * time.Second).Unix(),
		AccountID:      account,
	}
}

// Exchange swaps an authorization code plus PKCE verifier for stored tokens.
// issuedClientID is the client ID from the callback (never
// dynamic_agent_client); redirectURI must exactly match the authorize request.
func Exchange(ctx context.Context, do Doer, code, verifier, issuedClientID, redirectURI string) (Tokens, time.Time, error) {
	return ExchangeWithHost(ctx, do, code, verifier, issuedClientID, redirectURI, "")
}

// ExchangeWithHost is Exchange plus the stable host ID to persist.
func ExchangeWithHost(ctx context.Context, do Doer, code, verifier, issuedClientID, redirectURI, hostID string) (Tokens, time.Time, error) {
	if issuedClientID == "" || issuedClientID == DynamicClientID {
		return Tokens{}, time.Time{}, fmt.Errorf("exchange requires the issued client_id from the callback, not dynamic_agent_client")
	}
	if redirectURI == "" {
		return Tokens{}, time.Time{}, fmt.Errorf("exchange requires the same redirect_uri as the authorization request")
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", issuedClientID)
	form.Set("redirect_uri", redirectURI)
	form.Set("code", code)
	form.Set("code_verifier", verifier)
	form.Set("resource", Resource)
	reply, err := postForm(ctx, do, form)
	if err != nil {
		return Tokens{}, time.Time{}, err
	}
	savedAt := time.Now()
	t := tokensFromReply(reply, issuedClientID, hostID, "", reply.Email, "", savedAt)
	redactTokens(t)
	return t, savedAt, nil
}

// ExchangeValidated combines Exchange with ID-token validation and plan-scope
// enforcement. It verifies the returned ID token against JWKS (issuer,
// audience=issued client, expiry, nonce), derives subject/email/issuer from
// the validated claims, and rebuilds the credential with that identity. A
// valid ID token without the plan scope keeps the sign-in but reports it as
// disabled via ErrPlanNotEnabled.
func ExchangeValidated(ctx context.Context, do Doer, jwks *JWKS, code, verifier, issuedClientID, redirectURI, hostID, nonce string, now time.Time) (Tokens, error) {
	tokens, savedAt, err := ExchangeWithHost(ctx, do, code, verifier, issuedClientID, redirectURI, hostID)
	if err != nil {
		return Tokens{}, err
	}
	if tokens.IDToken == "" {
		return Tokens{}, fmt.Errorf("token response did not include an ID token")
	}
	if jwks == nil {
		fetched, err := FetchJWKS(ctx, do)
		if err != nil {
			return Tokens{}, err
		}
		jwks = fetched
	}
	claims, err := VerifyIDToken(tokens.IDToken, issuedClientID, nonce, jwks, now)
	if err != nil {
		return Tokens{}, err
	}
	final := tokensFromReply(tokenReply{
		AccessToken:  tokens.AccessToken,
		RefreshToken: tokens.RefreshToken,
		ExpiresIn:    tokens.ExpiresIn,
		TokenType:    tokens.TokenType,
		Scope:        strings.Join(tokens.Scopes, " "),
		IDToken:      tokens.IDToken,
		Email:        tokens.Email,
	}, issuedClientID, hostID, claims.Subject, claims.Email, claims.Issuer, savedAt)
	redactTokens(final)
	if !final.HasPlanScope() {
		return final, &ErrPlanNotEnabled{Scopes: final.Scopes, tokens: final}
	}
	return final, nil
}

// ErrPlanNotEnabled marks a successful OAuth exchange whose granted scopes
// lack chatgpt.tokens.use.direct. The sign-in is retained but plan usage is
// disabled; the caller should offer re-consent or another billing path.
type ErrPlanNotEnabled struct {
	Scopes []string
	tokens Tokens
}

func (e *ErrPlanNotEnabled) Error() string {
	return "chatgpt plan use isn't enabled (missing chatgpt.tokens.use.direct); re-authorize with the full scope set or configure another billing path"
}

// Tokens returns the retained sign-in without plan permission.
func (e *ErrPlanNotEnabled) Tokens() Tokens { return e.tokens }

// Refresh swaps a refresh token for new tokens. issuedClientID is the saved
// client for this token set (never dynamic_agent_client); scope is omitted to
// retain the grant.
func Refresh(ctx context.Context, do Doer, refreshToken, issuedClientID string) (Tokens, error) {
	return RefreshWithHost(ctx, do, refreshToken, issuedClientID, "", Tokens{})
}

// RefreshWithHost preserves identity/host metadata across rotation. prev
// supplies fallback subject/email/client/host/id-token when the refresh reply
// omits them. A replacement refresh token is stored; when the reply omits it
// the previous value is retained. Granted scopes and expiry are replaced
// together with the tokens.
func RefreshWithHost(ctx context.Context, do Doer, refreshToken, issuedClientID, hostID string, prev Tokens) (Tokens, error) {
	if refreshToken == "" {
		return Tokens{}, fmt.Errorf("no refresh token; run `q-agent login chatgpt` again")
	}
	if issuedClientID == "" || issuedClientID == DynamicClientID {
		return Tokens{}, fmt.Errorf("refresh requires the saved issued client_id; run `q-agent login chatgpt` again")
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("client_id", issuedClientID)
	form.Set("refresh_token", refreshToken)
	form.Set("resource", Resource)
	reply, err := postForm(ctx, do, form)
	if err != nil {
		return Tokens{}, err
	}
	if reply.RefreshToken == "" {
		reply.RefreshToken = refreshToken
	}
	savedAt := time.Now()
	host := hostID
	if host == "" {
		host = prev.ExtAgentHostID
	}
	subject := prev.Subject
	email := prev.Email
	issuer := prev.Issuer
	idToken := prev.IDToken
	if reply.IDToken != "" {
		idToken = reply.IDToken
	}
	if idToken != "" {
		if claims, err := decodeIDTokenClaims(idToken); err == nil {
			if claims.Subject != "" {
				subject = claims.Subject
			}
			if claims.Email != "" {
				email = claims.Email
			} else if reply.Email != "" {
				email = reply.Email
			}
			if claims.Issuer != "" {
				issuer = claims.Issuer
			}
		} else if reply.Email != "" && email == "" {
			email = reply.Email
		}
	} else if reply.Email != "" && email == "" {
		email = reply.Email
	}
	t := tokensFromReply(tokenReply{
		AccessToken:  reply.AccessToken,
		RefreshToken: reply.RefreshToken,
		ExpiresIn:    reply.ExpiresIn,
		TokenType:    reply.TokenType,
		Scope:        reply.Scope,
		IDToken:      idToken,
		Email:        email,
	}, issuedClientID, host, subject, email, issuer, savedAt)
	if len(t.Scopes) == 0 && len(prev.Scopes) > 0 && reply.Scope == "" {
		// Omit-scope refresh retains the grant; keep the previous grant list
		// only when the endpoint returned no scope value at all.
		t.Scopes = append([]string(nil), prev.Scopes...)
	}
	if t.Subject == "" {
		t.Subject = prev.Subject
	}
	if t.AccountID == "" {
		t.AccountID = accountIDFor(reply, prev.AccountID)
		if t.AccountID == "" {
			t.AccountID = t.Subject
		}
	}
	redactTokens(t)
	return t, nil
}

// Per-path refresh serialization: the default max_concurrent=2 lets two chats
// share one OAuth file. Without it both Load the same near-expiry tokens and
// send the same rotating refresh, and a single-use server rejects the second
// with invalid_grant. Atomic rename only protects file completeness, not the
// read/refresh/store transaction, so hold a process-wide lock per resolved
// path plus a best-effort cross-process file lock during the transaction.
var (
	refreshMu    sync.Mutex
	refreshLocks = map[string]*sync.Mutex{}
)

func lockFor(resolved string) *sync.Mutex {
	refreshMu.Lock()
	defer refreshMu.Unlock()
	if mu, ok := refreshLocks[resolved]; ok {
		return mu
	}
	mu := &sync.Mutex{}
	refreshLocks[resolved] = mu
	return mu
}

// lockMutexContext acquires mu or returns ctx.Err(). Polling with TryLock keeps
// startup/task deadlines and cancellation working while another goroutine
// holds the per-path lock.
func lockMutexContext(ctx context.Context, mu *sync.Mutex) error {
	if mu.TryLock() {
		return nil
	}
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if mu.TryLock() {
				return nil
			}
		}
	}
}

// withRefreshFileLock holds an exclusive advisory lock on path+".lock" while
// fn runs. It is best-effort across processes; intra-process safety comes
// from lockFor. Non-Linux platforms skip the syscall.
func withRefreshFileLock(resolved string, fn func() error) error {
	return withRefreshFileLockContext(context.Background(), resolved, fn)
}

// withRefreshFileLockContext is the cancellable variant. It uses a
// non-blocking flock with retry so a stale holder cannot wedge startup
// (EnsureValid 30s), /models (15s), or task cancellation forever. The fast
// path in GetValidTokens avoids taking this lock at all when the stored
// token is still fresh.
func withRefreshFileLockContext(ctx context.Context, resolved string, fn func() error) error {
	if runtime.GOOS != "linux" {
		if err := ctx.Err(); err != nil {
			return err
		}
		return fn()
	}
	dir := filepath.Dir(resolved)
	if err := os.MkdirAll(dir, 0700); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fn()
	}
	f, err := os.OpenFile(resolved+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fn()
	}
	defer f.Close()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			if err := ctx.Err(); err != nil {
				return err
			}
			return fn()
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			// Unexpected flock failure: best-effort fallback without the
			// cross-process lock, but still honor cancellation.
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fn()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func requireCurrent(t Tokens) error {
	if t.IsLegacy() {
		return fmt.Errorf("chatgpt login predates Sign in with ChatGPT (missing issued client_id); run `q-agent login chatgpt` again")
	}
	return nil
}

// GetValidTokens returns usable credentials, refreshing and persisting when
// the stored token is near expiry. A lock-free fast path returns fresh tokens
// without taking any lock; only the refresh transaction re-reads the file
// under the cancellable per-path + file locks so concurrent callers share one
// refresh and startup/task deadlines stay effective.
func GetValidTokens(ctx context.Context, do Doer, path string) (Tokens, error) {
	if err := ctx.Err(); err != nil {
		return Tokens{}, err
	}
	// Fast path: no locks, so a stuck sibling cannot block fresh credentials
	// and cannot wedge EnsureValid / Models deadlines.
	if t, err := Load(path); err == nil {
		if err := requireCurrent(t); err != nil {
			return Tokens{}, err
		}
		if !t.NeedsRefresh(time.Now()) {
			redactTokens(t)
			return t, nil
		}
	} else {
		// Load failures (missing file, bad mode, bad JSON) need no
		// serialization: report them without acquiring locks.
		// Fall through to the locked path only when the file exists but
		// needs a refresh; re-check there. To keep it simple and preserve
		// error text, return Load errors directly unless refresh is needed.
		// We cannot tell without the file, so return the error.
		// However a concurrent refresh may have just fixed it; the locked
		// path re-reads. Distinguish: if the error is "needs refresh"-like
		// it never is — Load never reports expiry. So return directly.
		// The only case that needs the lock is a present-but-stale file,
		// which Load returns successfully. Hence any Load error is final.
		return Tokens{}, err
	}
	full, err := ResolvePath(path)
	if err != nil {
		return Tokens{}, err
	}
	mu := lockFor(full)
	if err := lockMutexContext(ctx, mu); err != nil {
		return Tokens{}, err
	}
	defer mu.Unlock()
	var out Tokens
	lockErr := withRefreshFileLockContext(ctx, full, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		t, err := Load(path)
		if err != nil {
			return err
		}
		if err := requireCurrent(t); err != nil {
			return err
		}
		if !t.NeedsRefresh(time.Now()) {
			redactTokens(t)
			out = t
			return nil
		}
		if t.RefreshToken == "" {
			return fmt.Errorf("chatgpt token expired and no refresh token; run `q-agent login chatgpt` again")
		}
		next, err := RefreshWithHost(ctx, do, t.RefreshToken, t.ClientID, t.ExtAgentHostID, t)
		if err != nil {
			if _, ok := err.(*ErrInvalidGrant); ok {
				return fmt.Errorf("chatgpt token refresh rejected (invalid_grant); run `q-agent login chatgpt` again: %w", err)
			}
			return fmt.Errorf("chatgpt token refresh failed; run `q-agent login chatgpt` again: %w", err)
		}
		if err := Save(path, next); err != nil {
			return err
		}
		out = next
		return nil
	})
	if lockErr != nil {
		return Tokens{}, lockErr
	}
	return out, nil
}

// GetValidToken returns a usable access token, refreshing and persisting when
// the stored token is near expiry.
func GetValidToken(ctx context.Context, do Doer, path string) (string, error) {
	t, err := GetValidTokens(ctx, do, path)
	if err != nil {
		return "", err
	}
	return t.AccessToken, nil
}

// RefreshAndSaveWithStale forces a refresh (used for a 401 retry) but first
// re-reads under the cancellable per-path lock: if another goroutine already
// rotated past staleAccess, its fresh token is reused instead of spending the
// new refresh. A lock-free fast path covers that reuse case so 401 retries do
// not block on a stuck holder when the file is already fresh.
func RefreshAndSaveWithStale(ctx context.Context, do Doer, path, staleAccess string) (Tokens, error) {
	if err := ctx.Err(); err != nil {
		return Tokens{}, err
	}
	if staleAccess != "" {
		if t, err := Load(path); err == nil {
			if err := requireCurrent(t); err == nil {
				if t.AccessToken != staleAccess && !t.NeedsRefresh(time.Now()) {
					redactTokens(t)
					return t, nil
				}
			}
		}
	}
	full, err := ResolvePath(path)
	if err != nil {
		return Tokens{}, err
	}
	mu := lockFor(full)
	if err := lockMutexContext(ctx, mu); err != nil {
		return Tokens{}, err
	}
	defer mu.Unlock()
	var out Tokens
	lockErr := withRefreshFileLockContext(ctx, full, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		t, err := Load(path)
		if err != nil {
			return err
		}
		if err := requireCurrent(t); err != nil {
			return err
		}
		if staleAccess != "" && t.AccessToken != staleAccess && !t.NeedsRefresh(time.Now()) {
			redactTokens(t)
			out = t
			return nil
		}
		if t.RefreshToken == "" {
			return fmt.Errorf("no refresh token; run `q-agent login chatgpt` again")
		}
		next, err := RefreshWithHost(ctx, do, t.RefreshToken, t.ClientID, t.ExtAgentHostID, t)
		if err != nil {
			if _, ok := err.(*ErrInvalidGrant); ok {
				return fmt.Errorf("chatgpt token refresh rejected (invalid_grant); run `q-agent login chatgpt` again: %w", err)
			}
			return fmt.Errorf("chatgpt token refresh failed; run `q-agent login chatgpt` again: %w", err)
		}
		if err := Save(path, next); err != nil {
			return err
		}
		out = next
		return nil
	})
	if lockErr != nil {
		return Tokens{}, lockErr
	}
	return out, nil
}

// RefreshAndSave forces a refresh regardless of expiry (used for a 401 retry).
func RefreshAndSave(ctx context.Context, do Doer, path string) (string, error) {
	t, err := RefreshAndSaveWithStale(ctx, do, path, "")
	if err != nil {
		return "", err
	}
	return t.AccessToken, nil
}

// EnsureValid is the startup preflight: it loads the file and, when the
// access token is expired or near expiry, refreshes it now instead of letting
// the first user task fail. A missing/unusable file or a failed refresh
// reports `run q-agent login chatgpt` before serve starts.
func EnsureValid(ctx context.Context, do Doer, path string) (Tokens, error) {
	t, err := GetValidTokens(ctx, do, path)
	if err != nil {
		return Tokens{}, err
	}
	if t.AccessToken == "" {
		return Tokens{}, fmt.Errorf("chatgpt oauth file has no access token; run `q-agent login chatgpt` again")
	}
	return t, nil
}
