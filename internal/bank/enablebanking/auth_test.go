package enablebanking_test

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jarcoal/httpmock"
	"github.com/stretchr/testify/suite"

	"github.com/Toshik1978/firefly-jar/internal/bank"
	"github.com/Toshik1978/firefly-jar/internal/bank/enablebanking"
	"github.com/Toshik1978/firefly-jar/internal/httpclient"
	"github.com/Toshik1978/firefly-jar/internal/redact"
	"github.com/Toshik1978/firefly-jar/internal/state"
)

// hexState32 matches the 32 lowercase hex characters StartAuth must generate for "state" (T058
// design; crypto/rand per the T058 ruling).
var hexState32 = regexp.MustCompile(`^[0-9a-f]{32}$`)

// authRequestBody is the POST /auth body shape the AuthSuite decodes to assert StartAuth's request
// (research R3): valid_until, the fixed transactions/balances flags, the aspsp echo, the generated
// state and the caller-supplied redirect_url/psu_type.
type authRequestBody struct {
	Access struct {
		ValidUntil   string `json:"valid_until"`
		Transactions bool   `json:"transactions"`
		Balances     bool   `json:"balances"`
	} `json:"access"`
	Aspsp struct {
		Name    string `json:"name"`
		Country string `json:"country"`
	} `json:"aspsp"`
	State       string `json:"state"`
	RedirectURL string `json:"redirect_url"`
	PsuType     string `json:"psu_type"`
}

// sessionRequestBody is the POST /sessions body shape: only the authorization code (research R3).
type sessionRequestBody struct {
	Code string `json:"code"`
}

// AuthSuite covers the Enable Banking consent flow (T058, FR-002, FR-018, FR-022, research R2-R4):
// FindASPSP, StartAuth, ParseRedirect, CreateSession and DeleteSession. It is the RED half of
// T059; every case here fails today for lack of the production symbols it exercises.
type AuthSuite struct {
	suite.Suite

	key *rsa.PrivateKey
}

// SetupSuite parses the anonymized PKCS#8 test private key once for every test in the suite (the
// same key SignerSuite and TransactionsSuite use).
func (s *AuthSuite) SetupSuite() {
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

// newSigner builds a Signer anchored at a fixed instant, so every test in this suite gets a
// deterministic, reproducible bearer token instead of depending on the wall clock.
func (s *AuthSuite) newSigner() *enablebanking.Signer {
	now := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)

	return enablebanking.NewSigner(testAppID, s.key, func() time.Time { return now })
}

// fixture reads one anonymized testdata file, failing the test immediately if it is missing.
func (s *AuthSuite) fixture(name string) []byte {
	s.T().Helper()

	path := filepath.Clean(filepath.Join(fixtureDir, name))

	data, err := os.ReadFile(path)
	s.Require().NoError(err, "read fixture %s", path)

	return data
}

// requireNoPsuHeaders asserts that header carries no name starting with Psu- (account-information
// consent only, constitution §I).
func (s *AuthSuite) requireNoPsuHeaders(header http.Header) {
	for name := range header {
		s.Falsef(
			strings.HasPrefix(name, "Psu-"),
			"unexpected consent-only-violating header %s (constitution §I)",
			name,
		)
	}
}

// TestFindASPSPSendsExactQueryBearerAuthAndMatchesNameExactly asserts the T058 design's FindASPSP
// request shape: GET /aspsps?country=LT&psu_type=personal&service=AIS with a Bearer JWT and no
// Psu- header, and that "Swedbank" matches the fixture's exact entry, not the "Swedbankas"
// near-miss, carrying its own maximum_consent_validity through as a time.Duration.
func (s *AuthSuite) TestFindASPSPSendsExactQueryBearerAuthAndMatchesNameExactly() {
	signer := s.newSigner()

	wantToken, err := signer.Token()
	s.Require().NoError(err)

	body := s.fixture("aspsps.json")

	var (
		gotMethod string
		gotPath   string
		gotQuery  string
		gotHeader http.Header
	)

	transport := httpmock.NewMockTransport()
	transport.RegisterResponder(http.MethodGet, enableBankingTestURL+"/aspsps",
		func(req *http.Request) (*http.Response, error) {
			gotMethod = req.Method
			gotPath = req.URL.Path
			gotQuery = req.URL.RawQuery
			gotHeader = req.Header.Clone()

			return httpmock.NewBytesResponse(http.StatusOK, body), nil
		},
	)

	client := enablebanking.New(enableBankingTestURL, &http.Client{Transport: transport}, signer, redact.New())

	got, err := client.FindASPSP(context.Background(), "Swedbank", "LT", "personal")
	s.Require().NoError(err)

	s.Equal(http.MethodGet, gotMethod)
	s.Equal("/aspsps", gotPath)
	s.Equal("country=LT&psu_type=personal&service=AIS", gotQuery)
	s.Equal("Bearer "+wantToken, gotHeader.Get("Authorization"))
	s.requireNoPsuHeaders(gotHeader)

	s.Equal(enablebanking.ASPSP{
		Name:                   "Swedbank",
		Country:                "LT",
		MaximumConsentValidity: 15552000 * time.Second,
	}, got)
}

// TestFindASPSPUnknownBankListsCloseNames asserts that a name with no exact match in the fixture
// fails with an error naming the near-misses ("Swedbank" and "Swedbankas" both start with the
// typo'd "Swedban"), rather than silently picking one.
func (s *AuthSuite) TestFindASPSPUnknownBankListsCloseNames() {
	signer := s.newSigner()
	body := s.fixture("aspsps.json")

	transport := httpmock.NewMockTransport()
	transport.RegisterResponder(http.MethodGet, enableBankingTestURL+"/aspsps",
		httpmock.NewBytesResponder(http.StatusOK, body),
	)

	client := enablebanking.New(enableBankingTestURL, &http.Client{Transport: transport}, signer, redact.New())

	_, err := client.FindASPSP(context.Background(), "Swedban", "LT", "personal")
	s.Require().Error(err)
	s.Contains(err.Error(), "Swedbank")
	s.Contains(err.Error(), "Swedbankas")
}

// TestStartAuthPostsExpectedBodyAndReturnsURLAndState asserts the T058 design's StartAuth request
// body (valid_until = now + MaximumConsentValidity - 1h, the fixed transactions/balances flags,
// the aspsp echo, a 32-lowercase-hex state, and the caller's redirect_url/psu_type) and that the
// returned AuthStart carries the fixture's URL and the same state the request sent.
func (s *AuthSuite) TestStartAuthPostsExpectedBodyAndReturnsURLAndState() {
	signer := s.newSigner()
	body := s.fixture("auth_start.json")

	now := time.Date(2026, time.September, 23, 12, 0, 0, 0, time.UTC)
	aspsp := enablebanking.ASPSP{Name: "Swedbank", Country: "LT", MaximumConsentValidity: 15552000 * time.Second}

	var (
		gotMethod string
		gotPath   string
		gotHeader http.Header
		gotBody   authRequestBody
		decodeErr error
	)

	transport := httpmock.NewMockTransport()
	transport.RegisterResponder(http.MethodPost, enableBankingTestURL+"/auth",
		func(req *http.Request) (*http.Response, error) {
			gotMethod = req.Method
			gotPath = req.URL.Path
			gotHeader = req.Header.Clone()

			decodeErr = json.NewDecoder(req.Body).Decode(&gotBody)

			return httpmock.NewBytesResponse(http.StatusOK, body), nil
		},
	)

	client := enablebanking.New(enableBankingTestURL, &http.Client{Transport: transport}, signer, redact.New())

	got, err := client.StartAuth(context.Background(), aspsp, "https://example.com/eb-callback", "personal", now)
	s.Require().NoError(err)
	s.Require().NoError(decodeErr, "the request body must decode as JSON")

	s.Equal(http.MethodPost, gotMethod)
	s.Equal("/auth", gotPath)
	s.requireNoPsuHeaders(gotHeader)

	wantValidUntil := now.Add(aspsp.MaximumConsentValidity - time.Hour).Format(time.RFC3339)
	s.Equal(wantValidUntil, gotBody.Access.ValidUntil)
	s.True(gotBody.Access.Transactions)
	s.False(gotBody.Access.Balances)
	s.Equal("Swedbank", gotBody.Aspsp.Name)
	s.Equal("LT", gotBody.Aspsp.Country)
	s.Equal("https://example.com/eb-callback", gotBody.RedirectURL)
	s.Equal("personal", gotBody.PsuType)

	s.Regexp(hexState32, gotBody.State, "state must be 32 lowercase hex characters")

	s.Equal("https://example.com/auth/00000000-0000-0000-0000-000000000001", got.URL)
	s.Equal(gotBody.State, got.State, "the returned state must match what the request sent")
}

// TestParseRedirect covers every ParseRedirect case in the T058 design: a pasted redirect with
// surrounding whitespace yielding the code, a provider-side cancellation surfacing its
// description, a state mismatch mapping to bank.ErrStateMismatch, and a missing code failing.
func (s *AuthSuite) TestParseRedirect() {
	const expectedState = "deadbeefdeadbeefdeadbeefdeadbeef"

	s.Run("code extracted with surrounding whitespace tolerated", func() {
		pasted := "  https://example.com/cb?code=abc&state=" + expectedState + "  \n"

		code, err := enablebanking.ParseRedirect(pasted, expectedState)
		s.Require().NoError(err)
		s.Equal("abc", code)
	})

	s.Run("access denied surfaces the provider's description", func() {
		pasted := "https://example.com/cb?error=access_denied&error_description=Cancelled%20by%20user&state=" +
			expectedState

		_, err := enablebanking.ParseRedirect(pasted, expectedState)
		s.Require().Error(err)
		s.Contains(err.Error(), "Cancelled by user")
	})

	s.Run("state mismatch maps to bank.ErrStateMismatch", func() {
		pasted := "https://example.com/cb?code=abc&state=0000000000000000000000000000dead"

		_, err := enablebanking.ParseRedirect(pasted, expectedState)
		s.Require().Error(err)
		s.Require().ErrorIs(err, bank.ErrStateMismatch)
	})

	s.Run("missing code fails", func() {
		pasted := "https://example.com/cb?state=" + expectedState

		_, err := enablebanking.ParseRedirect(pasted, expectedState)
		s.Require().Error(err)
	})

	// The pasted URL carries the one-time authorization code, and auth prints this error to
	// stderr, so an unparsable paste must fail with fixed text that echoes nothing of the input.
	s.Run("an unparsable paste is not echoed", func() {
		const code = "0000onetimecode0000"

		pasted := "https://example.com:x/cb?code=" + code + "&state=" + expectedState

		_, err := enablebanking.ParseRedirect(pasted, expectedState)
		s.Require().Error(err)
		s.Equal("parse redirect: not a valid URL", err.Error())
		s.NotContains(err.Error(), code)
		s.NotContains(err.Error(), "example.com")
	})
}

// TestCreateSessionPostsCodeAndReturnsSessionExactly asserts the T058 design's CreateSession
// request ({"code": ...}) and that the response maps to state.Session exactly: Provider
// "enablebanking", the fixture's session id and valid_until, AuthorizedAt equal to the injected
// now, and both accounts -- including the IBAN-less one -- keyed by uid and identification_hash.
func (s *AuthSuite) TestCreateSessionPostsCodeAndReturnsSessionExactly() {
	signer := s.newSigner()
	body := s.fixture("session_created.json")

	now := time.Date(2026, time.September, 23, 12, 0, 0, 0, time.UTC)

	var (
		gotMethod string
		gotPath   string
		gotHeader http.Header
		gotBody   sessionRequestBody
		decodeErr error
	)

	transport := httpmock.NewMockTransport()
	transport.RegisterResponder(http.MethodPost, enableBankingTestURL+"/sessions",
		func(req *http.Request) (*http.Response, error) {
			gotMethod = req.Method
			gotPath = req.URL.Path
			gotHeader = req.Header.Clone()

			decodeErr = json.NewDecoder(req.Body).Decode(&gotBody)

			return httpmock.NewBytesResponse(http.StatusOK, body), nil
		},
	)

	client := enablebanking.New(enableBankingTestURL, &http.Client{Transport: transport}, signer, redact.New())

	got, err := client.CreateSession(context.Background(), "abc-code", now)
	s.Require().NoError(err)
	s.Require().NoError(decodeErr, "the request body must decode as JSON")

	s.Equal(http.MethodPost, gotMethod)
	s.Equal("/sessions", gotPath)
	s.Equal("abc-code", gotBody.Code)
	s.requireNoPsuHeaders(gotHeader)

	want := state.Session{
		Provider:     "enablebanking",
		SessionID:    "00000000-0000-0000-0000-000000000002",
		ValidUntil:   time.Date(2027, time.March, 22, 9, 0, 0, 0, time.UTC),
		AuthorizedAt: now,
		Accounts: []state.Account{
			{
				UID:      "00000000-0000-0000-0000-000000000003",
				Hash:     "0000000000000000000000000000000000000000000000000000000000000000",
				IBAN:     "LT000000000000000001",
				Currency: "EUR",
				Name:     "Example Checking Account",
			},
			{
				UID:      "00000000-0000-0000-0000-000000000004",
				Hash:     "1111111111111111111111111111111111111111111111111111111111111111",
				IBAN:     "",
				Currency: "EUR",
				Name:     "Example Card",
			},
		},
	}
	s.Equal(want, got)
}

// TestCreateSessionPostIsNotRetriedOn500 asserts the second half of the T058/T059 ruling: POST
// /sessions is not idempotent, so even wrapped in the production httpclient.RetryTransport a 500
// response is hit exactly once, never retried like a GET would be.
func (s *AuthSuite) TestCreateSessionPostIsNotRetriedOn500() {
	signer := s.newSigner()

	transport := httpmock.NewMockTransport()
	transport.RegisterResponder(http.MethodPost, enableBankingTestURL+"/sessions",
		httpmock.NewStringResponder(http.StatusInternalServerError, ""),
	)

	hc := httpclient.NewClient(&httpclient.RetryTransport{Base: transport})
	client := enablebanking.New(enableBankingTestURL, hc, signer, redact.New())

	_, err := client.CreateSession(context.Background(), "abc-code", time.Now())
	s.Require().Error(err)
	s.Equal(1, transport.GetTotalCallCount(), "a POST must never be retried by httpclient.RetryTransport")
}

// TestDeleteSessionSendsDelete asserts DeleteSession sends DELETE /sessions/{id}.
func (s *AuthSuite) TestDeleteSessionSendsDelete() {
	signer := s.newSigner()

	var (
		gotMethod string
		gotPath   string
		gotHeader http.Header
	)

	transport := httpmock.NewMockTransport()
	transport.RegisterResponder(
		http.MethodDelete, enableBankingTestURL+"/sessions/00000000-0000-0000-0000-000000000002",
		func(req *http.Request) (*http.Response, error) {
			gotMethod = req.Method
			gotPath = req.URL.Path
			gotHeader = req.Header.Clone()

			return httpmock.NewStringResponse(http.StatusNoContent, ""), nil
		},
	)

	client := enablebanking.New(enableBankingTestURL, &http.Client{Transport: transport}, signer, redact.New())

	err := client.DeleteSession(context.Background(), "00000000-0000-0000-0000-000000000002")
	s.Require().NoError(err)

	s.Equal(http.MethodDelete, gotMethod)
	s.Equal("/sessions/00000000-0000-0000-0000-000000000002", gotPath)
	s.requireNoPsuHeaders(gotHeader)
}

// TestClientImplementsAuthorizer is a compile-time-only assertion that *enablebanking.Client
// satisfies bank.Authorizer (T059 design). It lives inside a test method, not a package-level var,
// because package-level vars are banned (.claude/CLAUDE.md, gochecknoglobals); if the assertion
// ever stops compiling, this is where the build fails.
func (s *AuthSuite) TestClientImplementsAuthorizer() {
	var _ bank.Authorizer = (*enablebanking.Client)(nil)
}
