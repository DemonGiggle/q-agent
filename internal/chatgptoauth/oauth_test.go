package chatgptoauth

import (
	"strings"
	"testing"
	"time"
)

func TestPKCEChallengeVector(t *testing.T) {
	// RFC 7636 Appendix B vector.
	if got := Challenge("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"); got != "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" {
		t.Fatalf("challenge mismatch: %s", got)
	}
	v, err := NewVerifier()
	if err != nil {
		t.Fatal(err)
	}
	if len(v) < 43 || len(v) > 128 {
		t.Fatalf("verifier length %d out of range", len(v))
	}
	s, err := NewState()
	if err != nil {
		t.Fatal(err)
	}
	if s == "" {
		t.Fatal("empty state")
	}
	n, err := NewNonce()
	if err != nil {
		t.Fatal(err)
	}
	if n == "" {
		t.Fatal("empty nonce")
	}
}

func TestAuthorizeURLRoundTrip(t *testing.T) {
	u := AuthorizeURL(AuthorizeParams{
		ClientID:     DynamicClientID,
		RedirectURI:  CallbackURL(1455),
		State:        "state123",
		Nonce:        "nonce123",
		Verifier:     "verifier123",
		HostID:       "urn:uuid:host-1",
		IsNewAccount: true,
	})
	for _, want := range []string{
		"code_challenge_method=S256",
		"response_type=code",
		"client_id=" + DynamicClientID,
		"agent_name_hint=" + AgentNameHint,
		"ext_agent_host_id=",
		"resource=",
		"nonce=nonce123",
		"127.0.0.1",
		"chatgpt.tokens.use.direct",
		"resource.invoke",
	} {
		if !strings.Contains(u, want) {
			t.Fatalf("missing %q in %s", want, u)
		}
	}
	if strings.Contains(u, "localhost") {
		t.Fatalf("must use 127.0.0.1, not localhost: %s", u)
	}
	// Re-auth omits agent_name_hint and carries hints.
	u2 := AuthorizeURL(AuthorizeParams{
		ClientID:     "oaiapp_test",
		RedirectURI:  CallbackURL(54321),
		State:        "s",
		Nonce:        "n",
		Verifier:     "v",
		HostID:       "urn:uuid:host-1",
		IDTokenHint:  "idtok",
		LoginHint:    "user@example.com",
		IsNewAccount: false,
	})
	if strings.Contains(u2, "agent_name_hint") {
		t.Fatalf("reauth must omit agent_name_hint: %s", u2)
	}
	if !strings.Contains(u2, "id_token_hint=idtok") || !strings.Contains(u2, "oaiapp_test") {
		t.Fatalf("reauth missing hints: %s", u2)
	}
}

func TestParseCallback(t *testing.T) {
	got, err := ParseCallback("http://127.0.0.1:1455/auth/callback?code=ABC&state=XYZ&client_id=oaiapp_123&scope=a+b", "XYZ", "", true)
	if err != nil || got.Code != "ABC" || got.ClientID != "oaiapp_123" {
		t.Fatalf("parse=%+v err=%v", got, err)
	}
	if _, err := ParseCallback("http://127.0.0.1:1455/auth/callback?code=ABC&state=OTHER&client_id=oaiapp_123", "XYZ", "", true); err == nil {
		t.Fatal("state mismatch accepted")
	}
	if _, err := ParseCallback("http://127.0.0.1:1455/auth/callback?error=access_denied&state=XYZ", "XYZ", "", true); err == nil {
		t.Fatal("error param accepted")
	}
	if _, err := ParseCallback("http://127.0.0.1:1455/auth/callback?state=XYZ&client_id=oaiapp_123", "XYZ", "", true); err == nil {
		t.Fatal("missing code accepted")
	}
	// New registration without issued ID is incomplete.
	if _, err := ParseCallback("http://127.0.0.1:1455/auth/callback?code=ABC&state=XYZ", "XYZ", "", true); err == nil {
		t.Fatal("new registration without client_id accepted")
	}
	// dynamic_agent_client must never be saved.
	if _, err := ParseCallback("http://127.0.0.1:1455/auth/callback?code=ABC&state=XYZ&client_id=dynamic_agent_client", "XYZ", "", true); err == nil {
		t.Fatal("dynamic client accepted as issued ID")
	}
	// Re-auth may omit client_id (retain pending).
	got, err = ParseCallback("http://127.0.0.1:1455/auth/callback?code=ABC&state=XYZ", "XYZ", "oaiapp_kept", false)
	if err != nil || got.ClientID != "oaiapp_kept" {
		t.Fatalf("reauth retain=%+v err=%v", got, err)
	}
	// Re-auth with a different client is rejected.
	if _, err := ParseCallback("http://127.0.0.1:1455/auth/callback?code=ABC&state=XYZ&client_id=oaiapp_other", "XYZ", "oaiapp_kept", false); err == nil {
		t.Fatal("client mismatch accepted")
	}
}

func TestTokensExpiry(t *testing.T) {
	now := time.Now()
	fresh := Tokens{
		AccessToken:  "a",
		RefreshToken: "r",
		ExpiresIn:    3600,
		SavedAt:      now.UTC().Format(time.RFC3339),
		Scopes:       []string{PlanScope},
	}
	if fresh.NeedsRefresh(now) || fresh.Expired(now) {
		t.Fatal("fresh token flagged")
	}
	if !fresh.HasPlanScope() {
		t.Fatal("plan scope missing")
	}
	soon := Tokens{AccessToken: "a", RefreshToken: "r", ExpiresAt: now.Add(2 * time.Minute).Unix()}
	if !soon.NeedsRefresh(now) {
		t.Fatal("near-expiry token not flagged")
	}
	empty := Tokens{}
	if !empty.NeedsRefresh(now) || !empty.Expired(now) {
		t.Fatal("empty token not flagged")
	}
	legacy := Tokens{AccessToken: "a", RefreshToken: "r", ExpiresAt: now.Add(time.Hour).Unix(), AccountID: "acct-1"}
	if !legacy.IsLegacy() {
		t.Fatal("legacy token not detected")
	}
	current := Tokens{AccessToken: "a", RefreshToken: "r", ClientID: "oaiapp_1", IDToken: "id", Subject: "sub", SavedAt: now.UTC().Format(time.RFC3339), ExpiresIn: 3600}
	if current.IsLegacy() {
		t.Fatal("current token flagged as legacy")
	}
}

func TestCallbackURLHost(t *testing.T) {
	if got := CallbackURL(1455); got != "http://127.0.0.1:1455/auth/callback" {
		t.Fatalf("got %q", got)
	}
}
