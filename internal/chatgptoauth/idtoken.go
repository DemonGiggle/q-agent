package chatgptoauth

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// IDClaims are the validated identity claims needed for the SIWC record.
type IDClaims struct {
	Issuer  string
	Subject string
	Email   string
	Nonce   string
	Issued  time.Time
	Expiry  time.Time
}

// JWK is one RSA key from the JWKS document.
type JWK struct {
	Kid string
	N   *big.Int
	E   int
}

// JWKS is a set of keys by kid.
type JWKS struct {
	Keys map[string]JWK
}

var (
	jwksMu     sync.Mutex
	cachedJWKS *JWKS
	cachedAt   time.Time
)

const jwksTTL = time.Hour

func parseJWKSEponent(e string) (int, error) {
	raw, err := base64.RawURLEncoding.DecodeString(e)
	if err != nil {
		raw, err = base64.URLEncoding.DecodeString(e)
		if err != nil {
			return 0, err
		}
	}
	bi := new(big.Int).SetBytes(raw)
	if !bi.IsInt64() {
		return 0, fmt.Errorf("exponent out of range")
	}
	return int(bi.Int64()), nil
}

func parseJWKModulus(n string) (*big.Int, error) {
	raw, err := base64.RawURLEncoding.DecodeString(n)
	if err != nil {
		raw, err = base64.URLEncoding.DecodeString(n)
		if err != nil {
			return nil, err
		}
	}
	bi := new(big.Int).SetBytes(raw)
	if bi.Sign() <= 0 {
		return nil, fmt.Errorf("empty modulus")
	}
	return bi, nil
}

// ParseJWKS parses a JWKS JSON document with RSA keys.
func ParseJWKS(data []byte) (*JWKS, error) {
	var doc struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			Use string `json:"use"`
			Alg string `json:"alg"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse JWKS: %w", err)
	}
	out := &JWKS{Keys: map[string]JWK{}}
	for _, k := range doc.Keys {
		if k.Kty != "RSA" || k.N == "" || k.E == "" {
			continue
		}
		n, err := parseJWKModulus(k.N)
		if err != nil {
			continue
		}
		e, err := parseJWKSEponent(k.E)
		if err != nil {
			continue
		}
		if k.Kid == "" {
			continue
		}
		out.Keys[k.Kid] = JWK{Kid: k.Kid, N: n, E: e}
	}
	if len(out.Keys) == 0 {
		return nil, fmt.Errorf("JWKS contains no RSA keys")
	}
	return out, nil
}

// FetchJWKS loads OpenAI's signing keys, caching them for an hour. Unknown
// kids refresh once.
func FetchJWKS(ctx context.Context, do Doer) (*JWKS, error) {
	jwksMu.Lock()
	if cachedJWKS != nil && time.Since(cachedAt) < jwksTTL {
		defer jwksMu.Unlock()
		return cachedJWKS, nil
	}
	jwksMu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, JWKSURI, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := do.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch JWKS: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetch JWKS returned %s", resp.Status)
	}
	parsed, err := ParseJWKS(data)
	if err != nil {
		return nil, err
	}
	jwksMu.Lock()
	cachedJWKS = parsed
	cachedAt = time.Now()
	jwksMu.Unlock()
	return parsed, nil
}

// ClearJWKSCache resets the JWKS cache. Tests only.
func ClearJWKSCache() {
	jwksMu.Lock()
	cachedJWKS = nil
	cachedAt = time.Time{}
	jwksMu.Unlock()
}

func b64URLDecode(s string) ([]byte, error) {
	if raw, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return raw, nil
	}
	return base64.URLEncoding.DecodeString(s)
}

func decodeIDTokenClaims(idToken string) (IDClaims, error) {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return IDClaims{}, fmt.Errorf("ID token must have 3 segments")
	}
	raw, err := b64URLDecode(parts[1])
	if err != nil {
		return IDClaims{}, fmt.Errorf("decode ID token claims: %w", err)
	}
	var payload struct {
		Iss   string `json:"iss"`
		Sub   string `json:"sub"`
		Aud   any    `json:"aud"`
		Exp   int64  `json:"exp"`
		Iat   int64  `json:"iat"`
		Nonce string `json:"nonce"`
		Email string `json:"email"`
		Auth  struct {
			Salt string `json:"per_user_salt"`
		} `json:"https://api.openai.com/auth"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return IDClaims{}, fmt.Errorf("decode ID token claims: %w", err)
	}
	return IDClaims{
		Issuer:  payload.Iss,
		Subject: payload.Sub,
		Email:   payload.Email,
		Nonce:   payload.Nonce,
		Issued:  time.Unix(payload.Iat, 0),
		Expiry:  time.Unix(payload.Exp, 0),
	}, nil
}

// VerifyIDToken verifies the JWT signature against JWKS and checks issuer,
// audience (issued client ID), expiration, and the request nonce. It allows a
// small clock skew. On success it returns the identity claims.
func VerifyIDToken(idToken, expectedClientID, expectedNonce string, jwks *JWKS, now time.Time) (IDClaims, error) {
	if jwks == nil || len(jwks.Keys) == 0 {
		return IDClaims{}, fmt.Errorf("no JWKS keys for ID-token verification")
	}
	if expectedClientID == "" {
		return IDClaims{}, fmt.Errorf("ID-token verification requires the issued client_id")
	}
	if now.IsZero() {
		now = time.Now()
	}
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return IDClaims{}, fmt.Errorf("ID token must have 3 segments")
	}
	headerRaw, err := b64URLDecode(parts[0])
	if err != nil {
		return IDClaims{}, fmt.Errorf("decode ID token header: %w", err)
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
		Typ string `json:"typ"`
	}
	if err := json.Unmarshal(headerRaw, &header); err != nil {
		return IDClaims{}, fmt.Errorf("decode ID token header: %w", err)
	}
	if header.Alg != "RS256" {
		return IDClaims{}, fmt.Errorf("unexpected ID token alg %q, want RS256", header.Alg)
	}
	key, ok := jwks.Keys[header.Kid]
	if !ok {
		return IDClaims{}, fmt.Errorf("unknown ID token kid %q", header.Kid)
	}
	signingInput := parts[0] + "." + parts[1]
	sig, err := b64URLDecode(parts[2])
	if err != nil {
		return IDClaims{}, fmt.Errorf("decode ID token signature: %w", err)
	}
	digest := sha256.Sum256([]byte(signingInput))
	pub := rsa.PublicKey{N: key.N, E: key.E}
	if err := rsa.VerifyPKCS1v15(&pub, crypto.SHA256, digest[:], sig); err != nil {
		return IDClaims{}, fmt.Errorf("invalid ID token signature: %w", err)
	}

	payloadRaw, err := b64URLDecode(parts[1])
	if err != nil {
		return IDClaims{}, fmt.Errorf("decode ID token claims: %w", err)
	}
	var payload struct {
		Iss   string `json:"iss"`
		Sub   string `json:"sub"`
		Exp   int64  `json:"exp"`
		Iat   int64  `json:"iat"`
		Nonce string `json:"nonce"`
		Email string `json:"email"`
	}
	if err := json.Unmarshal(payloadRaw, &payload); err != nil {
		return IDClaims{}, fmt.Errorf("decode ID token claims: %w", err)
	}
	// Audience may be a string or an array (see website sample with ["oaiapp_..."]).
	var aud []string
	var audRaw struct {
		Aud any `json:"aud"`
	}
	if err := json.Unmarshal(payloadRaw, &audRaw); err == nil {
		switch v := audRaw.Aud.(type) {
		case string:
			aud = []string{v}
		case []any:
			for _, e := range v {
				if s, ok := e.(string); ok {
					aud = append(aud, s)
				}
			}
		}
	}
	if payload.Iss != Issuer {
		return IDClaims{}, fmt.Errorf("invalid ID token issuer %q, want %q", payload.Iss, Issuer)
	}
	found := false
	for _, a := range aud {
		if a == expectedClientID {
			found = true
			break
		}
	}
	if !found {
		return IDClaims{}, fmt.Errorf("invalid ID token audience, want %q", expectedClientID)
	}
	const skew = 5 * time.Minute
	if payload.Exp == 0 || now.Add(skew).Unix() >= int64(0) && now.After(time.Unix(payload.Exp, 0).Add(skew)) {
		return IDClaims{}, fmt.Errorf("ID token is expired")
	}
	if payload.Iat != 0 && time.Unix(payload.Iat, 0).After(now.Add(skew)) {
		return IDClaims{}, fmt.Errorf("ID token issued in the future")
	}
	if expectedNonce != "" && payload.Nonce != expectedNonce {
		return IDClaims{}, fmt.Errorf("ID token nonce mismatch")
	}
	if payload.Sub == "" {
		return IDClaims{}, fmt.Errorf("ID token has no subject")
	}
	return IDClaims{
		Issuer:  payload.Iss,
		Subject: payload.Sub,
		Email:   payload.Email,
		Nonce:   payload.Nonce,
		Issued:  time.Unix(payload.Iat, 0),
		Expiry:  time.Unix(payload.Exp, 0),
	}, nil
}

// Revoke ends the renewable session. It discovers the revocation endpoint
// from openid-configuration, then POSTs token + hint + issued client. An empty
// HTTP 200 is success, including for an already-invalid token.
func Revoke(ctx context.Context, do Doer, refreshToken, issuedClientID string) error {
	if refreshToken == "" || issuedClientID == "" {
		return fmt.Errorf("revocation requires a refresh token and issued client_id")
	}
	endpoint := ""
	if req, err := http.NewRequestWithContext(ctx, http.MethodGet, OpenIDConfiguration, nil); err == nil {
		req.Header.Set("Accept", "application/json")
		if resp, err := do.Do(req); err == nil {
			data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				var doc struct {
					Revocation string `json:"revocation_endpoint"`
				}
				if json.Unmarshal(data, &doc) == nil {
					endpoint = doc.Revocation
				}
			}
		}
	}
	if endpoint == "" {
		return fmt.Errorf("discovery did not advertise a revocation endpoint")
	}
	form := map[string]string{
		"token":           refreshToken,
		"token_type_hint": "refresh_token",
		"client_id":       issuedClientID,
	}
	values := make(map[string][]string, len(form))
	_ = values
	body := "token=" + escape(refreshToken) + "&token_type_hint=refresh_token&client_id=" + escape(issuedClientID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := do.Do(req)
	if err != nil {
		return fmt.Errorf("revocation: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("revocation returned %s", resp.Status)
	}
	return nil
}

func escape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(s, "%", "%25"), "&", "%26"), "=", "%3D")
}
