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
}

func TestAuthorizeURLRoundTrip(t *testing.T) {
	u := AuthorizeURL("verifier123", "state123")
	if !strings.Contains(u, "code_challenge_method=S256") {
		t.Fatalf("missing S256: %s", u)
	}
	if !strings.Contains(u, ClientID) || !strings.Contains(u, "response_type=code") {
		t.Fatalf("missing fields: %s", u)
	}
}

func TestParseCallback(t *testing.T) {
	code, err := ParseCallback("http://localhost:1455/auth/callback?code=ABC&state=XYZ", "XYZ")
	if err != nil || code != "ABC" {
		t.Fatalf("parse=%q err=%v", code, err)
	}
	if _, err := ParseCallback("http://localhost:1455/auth/callback?code=ABC&state=OTHER", "XYZ"); err == nil {
		t.Fatal("state mismatch accepted")
	}
	if _, err := ParseCallback("http://localhost:1455/auth/callback?error=access_denied", ""); err == nil {
		t.Fatal("error param accepted")
	}
	if _, err := ParseCallback("http://localhost:1455/auth/callback?state=XYZ", "XYZ"); err == nil {
		t.Fatal("missing code accepted")
	}
}

func TestTokensExpiry(t *testing.T) {
	now := time.Now()
	fresh := Tokens{AccessToken: "a", RefreshToken: "r", ExpiresAt: now.Add(time.Hour).Unix()}
	if fresh.NeedsRefresh(now) || fresh.Expired(now) {
		t.Fatal("fresh token flagged")
	}
	soon := Tokens{AccessToken: "a", RefreshToken: "r", ExpiresAt: now.Add(2 * time.Minute).Unix()}
	if !soon.NeedsRefresh(now) {
		t.Fatal("near-expiry token not flagged")
	}
	empty := Tokens{}
	if !empty.NeedsRefresh(now) || !empty.Expired(now) {
		t.Fatal("empty token not flagged")
	}
}
