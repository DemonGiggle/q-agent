package chatgptoauth

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"strings"
	"testing"
	"time"
)

func testRSAKey(t *testing.T) (*rsa.PrivateKey, JWK) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	n := base64.RawURLEncoding.EncodeToString(priv.N.Bytes())
	eBytes := big.NewInt(int64(priv.E)).Bytes()
	e := base64.RawURLEncoding.EncodeToString(eBytes)
	doc, _ := json.Marshal(map[string]any{
		"keys": []map[string]string{
			{"kty": "RSA", "kid": "test-kid", "n": n, "e": e},
		},
	})
	jwks, err := ParseJWKS(doc)
	if err != nil {
		t.Fatal(err)
	}
	return priv, jwks.Keys["test-kid"]
}

func signTestIDToken(t *testing.T, priv *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"test-kid","typ":"JWT"}`))
	raw, _ := json.Marshal(claims)
	payload := base64.RawURLEncoding.EncodeToString(raw)
	input := header + "." + payload
	digest := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func TestVerifyIDTokenRoundTrip(t *testing.T) {
	priv, key := testRSAKey(t)
	jwks := &JWKS{Keys: map[string]JWK{"test-kid": key}}
	now := time.Now()
	claims := map[string]any{
		"iss":   Issuer,
		"sub":   "sub-123",
		"aud":   "oaiapp_test123",
		"exp":   now.Add(time.Hour).Unix(),
		"iat":   now.Unix(),
		"nonce": "n-1",
		"email": "user@example.com",
	}
	tok := signTestIDToken(t, priv, claims)
	got, err := VerifyIDToken(tok, "oaiapp_test123", "n-1", jwks, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.Subject != "sub-123" || got.Email != "user@example.com" {
		t.Fatalf("claims = %+v", got)
	}
	// Audience as array (website sample shape).
	claims["aud"] = []string{"oaiapp_test123"}
	tok = signTestIDToken(t, priv, claims)
	if _, err := VerifyIDToken(tok, "oaiapp_test123", "n-1", jwks, now); err != nil {
		t.Fatalf("array aud rejected: %v", err)
	}
}

func TestVerifyIDTokenRejects(t *testing.T) {
	priv, key := testRSAKey(t)
	jwks := &JWKS{Keys: map[string]JWK{"test-kid": key}}
	now := time.Now()
	base := map[string]any{
		"iss": Issuer, "sub": "s", "aud": "oaiapp_ok",
		"exp": now.Add(time.Hour).Unix(), "iat": now.Unix(), "nonce": "n",
	}
	if _, err := VerifyIDToken(signTestIDToken(t, priv, base), "oaiapp_other", "n", jwks, now); err == nil {
		t.Fatal("wrong audience accepted")
	}
	bad := map[string]any{
		"iss": "https://evil.example", "sub": "s", "aud": "oaiapp_ok",
		"exp": now.Add(time.Hour).Unix(), "iat": now.Unix(), "nonce": "n",
	}
	if _, err := VerifyIDToken(signTestIDToken(t, priv, bad), "oaiapp_ok", "n", jwks, now); err == nil {
		t.Fatal("wrong issuer accepted")
	}
	expired := map[string]any{
		"iss": Issuer, "sub": "s", "aud": "oaiapp_ok",
		"exp": now.Add(-time.Hour).Unix(), "iat": now.Add(-2 * time.Hour).Unix(), "nonce": "n",
	}
	if _, err := VerifyIDToken(signTestIDToken(t, priv, expired), "oaiapp_ok", "n", jwks, now); err == nil {
		t.Fatal("expired accepted")
	}
	nonceBad := map[string]any{
		"iss": Issuer, "sub": "s", "aud": "oaiapp_ok",
		"exp": now.Add(time.Hour).Unix(), "iat": now.Unix(), "nonce": "other",
	}
	if _, err := VerifyIDToken(signTestIDToken(t, priv, nonceBad), "oaiapp_ok", "n", jwks, now); err == nil {
		t.Fatal("nonce mismatch accepted")
	}
	// Tampered signature.
	tok := signTestIDToken(t, priv, base)
	parts := strings.Split(tok, ".")
	tampered := parts[0] + "." + parts[1] + ".AAAA"
	if _, err := VerifyIDToken(tampered, "oaiapp_ok", "n", jwks, now); err == nil {
		t.Fatal("bad signature accepted")
	}
}

func TestEnsureHostIDStable(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/host-id"
	first, err := EnsureHostID(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first, "urn:uuid:") {
		t.Fatalf("host id = %q", first)
	}
	second, err := EnsureHostID(p)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("host id changed: %q vs %q", first, second)
	}
}
