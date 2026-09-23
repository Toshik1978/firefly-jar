package enablebanking_test

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/stretchr/testify/suite"

	"github.com/Toshik1978/firefly-jar/internal/bank/enablebanking"
)

// testAppID is the fixed, non-secret app id used across the fixtures (research.md R2). It is a
// zeroed UUID, never a real Enable Banking application id.
const testAppID = "00000000-0000-0000-0000-000000000000"

// testKeyPath is the anonymized PKCS#8 test private key checked in for tests. It is never a live
// credential (.claude/CLAUDE.md "What never goes into a tracked file").
const testKeyPath = "../../../testdata/config/secrets/enablebanking.pem"

// fakeClock is an injectable "now" for Signer, so the suite can advance time deterministically
// instead of sleeping (data-model.md, research R2: cache reused until 5 minutes before exp).
type fakeClock struct {
	t time.Time
}

// now satisfies the func() time.Time the controller-fixed Signer constructor takes.
func (c *fakeClock) now() time.Time {
	return c.t
}

// advance moves the fake clock forward by d.
func (c *fakeClock) advance(d time.Duration) {
	c.t = c.t.Add(d)
}

// SignerSuite covers internal/bank/enablebanking.Signer (T026, research R2): the JWT header and
// claim shape, the RS256 signature, and the 5-minutes-before-exp reuse/resign cache policy.
type SignerSuite struct {
	suite.Suite

	key *rsa.PrivateKey
}

// SetupSuite parses the anonymized PKCS#8 test key once for every test in the suite.
func (s *SignerSuite) SetupSuite() {
	path := filepath.Clean(testKeyPath)

	raw, err := os.ReadFile(path)
	s.Require().NoError(err, "read test key %s", path)

	block, _ := pem.Decode(raw)
	s.Require().NotNil(block, "decode PEM from %s", path)

	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	s.Require().NoError(err, "parse PKCS8 key from %s", path)

	key, ok := parsed.(*rsa.PrivateKey)
	s.Require().True(ok, "test key is not an RSA private key")

	s.key = key
}

// splitToken splits a compact JWT into its three raw (still base64url-encoded) parts, failing the
// test if the shape is wrong.
func (s *SignerSuite) splitToken(token string) (header, payload, signature string) {
	s.T().Helper()

	parts := strings.Split(token, ".")
	s.Require().Len(parts, 3, "token %q must have exactly three dot-separated parts", token)

	return parts[0], parts[1], parts[2]
}

// decodeSegment base64url-decodes (no padding) one JWT segment.
func (s *SignerSuite) decodeSegment(segment string) []byte {
	s.T().Helper()

	decoded, err := base64.RawURLEncoding.DecodeString(segment)
	s.Require().NoError(err, "decode segment %q", segment)

	return decoded
}

// decodeClaims decodes a JWT payload segment into a claims map.
func (s *SignerSuite) decodeClaims(segment string) map[string]any {
	s.T().Helper()

	var claims map[string]any

	err := json.Unmarshal(s.decodeSegment(segment), &claims)
	s.Require().NoError(err, "unmarshal claims from segment %q", segment)

	return claims
}

// verifySignature checks that signature is a valid RS256 signature, over the exact
// "<header>.<payload>" signing input, made with the private half of s.key.
func (s *SignerSuite) verifySignature(header, payload, signature string) {
	s.T().Helper()

	sig := s.decodeSegment(signature)
	hashed := sha256.Sum256([]byte(header + "." + payload))

	pub, ok := s.key.Public().(*rsa.PublicKey)
	s.Require().True(ok, "test key public half is not an RSA public key")

	err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, hashed[:], sig)
	s.Require().NoError(err, "RS256 signature does not verify against the test public key")
}

// TestTokenHasThreeBase64URLParts asserts a signed token is a compact JWT: three dot-separated,
// base64url-decodable parts.
func (s *SignerSuite) TestTokenHasThreeBase64URLParts() {
	clock := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	signer := enablebanking.NewSigner(testAppID, s.key, clock.now)

	token, err := signer.Token()
	s.Require().NoError(err)

	header, payload, signature := s.splitToken(token)
	s.NotEmpty(s.decodeSegment(header))
	s.NotEmpty(s.decodeSegment(payload))
	s.NotEmpty(s.decodeSegment(signature))
}

// TestHeaderIsExactJSON asserts the header decodes to exactly {"typ":"JWT","alg":"RS256","kid":
// "<app_id>"}, byte for byte, in that key order (research R2).
func (s *SignerSuite) TestHeaderIsExactJSON() {
	clock := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	signer := enablebanking.NewSigner(testAppID, s.key, clock.now)

	token, err := signer.Token()
	s.Require().NoError(err)

	header, _, _ := s.splitToken(token)
	want := `{"typ":"JWT","alg":"RS256","kid":"` + testAppID + `"}`
	s.Equal(want, string(s.decodeSegment(header)))
}

// TestClaimsIssAudAndExpiry asserts the claims carry the fixed issuer and audience, and that exp is
// exactly one hour (3600 seconds) after iat (research R2).
func (s *SignerSuite) TestClaimsIssAudAndExpiry() {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: now}
	signer := enablebanking.NewSigner(testAppID, s.key, clock.now)

	token, err := signer.Token()
	s.Require().NoError(err)

	_, payload, _ := s.splitToken(token)
	claims := s.decodeClaims(payload)

	s.Equal("enablebanking.com", claims["iss"])
	s.Equal("api.enablebanking.com", claims["aud"])

	iat, ok := claims["iat"].(float64)
	s.Require().True(ok, "iat must be a JSON number")
	s.InDelta(float64(now.Unix()), iat, 0)

	exp, ok := claims["exp"].(float64)
	s.Require().True(ok, "exp must be a JSON number")
	s.InDelta(exp, iat+3600, 0)
}

// TestSignatureVerifiesWithPublicKey asserts the signature is a genuine RS256 signature over the
// header and payload, checkable with only the public half of the signing key.
func (s *SignerSuite) TestSignatureVerifiesWithPublicKey() {
	clock := &fakeClock{t: time.Date(2026, 3, 10, 9, 30, 0, 0, time.UTC)}
	signer := enablebanking.NewSigner(testAppID, s.key, clock.now)

	token, err := signer.Token()
	s.Require().NoError(err)

	header, payload, signature := s.splitToken(token)
	s.verifySignature(header, payload, signature)
}

// TestTokenIsCachedUntilFiveMinutesBeforeExpiry asserts the cache policy from research R2: the same
// token is returned up to (but not including) 5 minutes before its exp, and a fresh token — with a
// new iat — is signed from that point on.
func (s *SignerSuite) TestTokenIsCachedUntilFiveMinutesBeforeExpiry() {
	start := time.Date(2026, 9, 23, 8, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: start}
	signer := enablebanking.NewSigner(testAppID, s.key, clock.now)

	first, err := signer.Token()
	s.Require().NoError(err)

	// exp = start + 1h, so start + 54m59s is one second before the 5-minute-early cutoff: the
	// cached token must still be returned unchanged.
	clock.advance(54*time.Minute + 59*time.Second)

	stillCached, err := signer.Token()
	s.Require().NoError(err)
	s.Equal(first, stillCached, "token must still be cached at 54m59s")

	// One more second reaches start + 55m, exactly 5 minutes before exp: the signer must resign
	// with a new iat.
	clock.advance(1 * time.Second)

	refreshed, err := signer.Token()
	s.Require().NoError(err)
	s.NotEqual(first, refreshed, "token must be resigned at the 5-minutes-before-exp cutoff")

	_, payload, _ := s.splitToken(refreshed)
	claims := s.decodeClaims(payload)

	iat, ok := claims["iat"].(float64)
	s.Require().True(ok, "iat must be a JSON number")
	s.InDelta(float64(start.Add(55*time.Minute).Unix()), iat, 0)
}

// TestTokenSigningFailureIsWrapped asserts Token never swallows a real signing failure: with a key
// too small for RS256 (crypto/rsa itself refuses to sign with it, regardless of the SHA-256 digest
// size), Token returns a wrapped, non-nil error and no token, instead of ("", nil) or a panic.
func (s *SignerSuite) TestTokenSigningFailureIsWrapped() {
	// A hand-built key with a 513-bit modulus: mathematically not a real key pair (D is
	// arbitrary), but crypto/rsa rejects it purely on key size before any signing math runs, so
	// no real, larger key is spent to exercise this path.
	tooSmall := &rsa.PrivateKey{D: big.NewInt(12345)}
	tooSmall.N = new(big.Int).Lsh(big.NewInt(1), 512)
	tooSmall.E = 65537

	clock := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	signer := enablebanking.NewSigner(testAppID, tooSmall, clock.now)

	token, err := signer.Token()

	s.Require().Error(err)
	s.Empty(token)
}
