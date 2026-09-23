// Package enablebanking is the adapter for the Enable Banking API (constitution §IV): account
// information only. Nothing in this package calls a payment endpoint or sends a Psu-* header, and
// the tool runs unattended.
package enablebanking

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// Fixed shape of the Enable Banking JWT (research R2): a one-hour lifetime, reused until 5 minutes
// before it expires so every run does not re-sign on every request.
const (
	issuer      = "enablebanking.com"
	audience    = "api.enablebanking.com"
	tokenTTL    = time.Hour
	renewBefore = 5 * time.Minute
)

// claims is the JWT payload Enable Banking expects: issuer, audience and a one-hour validity
// window anchored to the injected clock (research R2).
type claims struct {
	Iss string `json:"iss"`
	Aud string `json:"aud"`
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
}

// Signer signs Enable Banking's RS256 application JWTs (research R2) and caches the result until
// it is within renewBefore of expiring. now is injected so tests never depend on the wall clock.
type Signer struct {
	appID string
	key   *rsa.PrivateKey
	now   func() time.Time

	// mu guards the cache. A run drives the signer from a single goroutine, but the mutex is
	// cheap insurance against that changing.
	mu      sync.Mutex
	token   string
	expires time.Time
}

// NewSigner builds a Signer for appID, signing with key and reading the time from now.
func NewSigner(appID string, key *rsa.PrivateKey, now func() time.Time) *Signer {
	return &Signer{
		appID: appID,
		key:   key,
		now:   now,
	}
}

// Token returns a signed Enable Banking application JWT, reusing the cached one until it is within
// 5 minutes of expiry, then signing a fresh one. The key never appears in a returned error.
func (s *Signer) Token() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	if s.token != "" && now.Before(s.expires.Add(-renewBefore)) {
		return s.token, nil
	}

	token, expires, err := s.sign(now)
	if err != nil {
		return "", fmt.Errorf("sign enable banking jwt: %w", err)
	}

	s.token = token
	s.expires = expires

	return s.token, nil
}

// sign builds and signs one JWT anchored at now, without touching the cache.
func (s *Signer) sign(now time.Time) (string, time.Time, error) {
	// Built as a literal, not json.Marshal, so the key order is byte-exact regardless of struct
	// field order or encoding/json's own choices (research R2).
	header := fmt.Sprintf(`{"typ":"JWT","alg":"RS256","kid":%q}`, s.appID)

	expires := now.Add(tokenTTL)

	payload, err := json.Marshal(claims{
		Iss: issuer,
		Aud: audience,
		Iat: now.Unix(),
		Exp: expires.Unix(),
	})
	if err != nil {
		return "", time.Time{}, fmt.Errorf("marshal claims: %w", err)
	}

	signingInput := encodeSegment([]byte(header)) + "." + encodeSegment(payload)

	digest := sha256.Sum256([]byte(signingInput))

	signature, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", time.Time{}, fmt.Errorf("rsa sign pkcs1v15: %w", err)
	}

	return signingInput + "." + encodeSegment(signature), expires, nil
}

// encodeSegment base64url-encodes one JWT segment without padding.
func encodeSegment(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}
