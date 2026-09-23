package enablebanking_test

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/stretchr/testify/suite"

	"github.com/Toshik1978/firefly-jar/internal/bank"
	"github.com/Toshik1978/firefly-jar/internal/bank/enablebanking"
	"github.com/Toshik1978/firefly-jar/internal/civil"
	"github.com/Toshik1978/firefly-jar/internal/domain"
	"github.com/Toshik1978/firefly-jar/internal/httpx"
	"github.com/Toshik1978/firefly-jar/internal/redact"
)

// fixtureDir holds the anonymized Enable Banking response bodies T028 built (relative to this
// package directory).
const fixtureDir = "../../../testdata/enablebanking"

// paginationCap is the pagination safety cap from research.md R5: an account that never reaches a
// null continuation_key within this many pages is reported as bank.ErrDataIncomplete instead of
// looping forever.
const paginationCap = 100

// TransactionsSuite covers (*enablebanking.Client).Transactions (T029, FR-002, FR-005, FR-005a,
// FR-005b, research R5-R7): the request shape, pagination, status mapping, date precedence, amount
// sign rules, description precedence, the flat and grouped response shapes, and error mapping. It
// is the RED half of T030; every case here fails for lack of a production Client today.
type TransactionsSuite struct {
	suite.Suite

	key *rsa.PrivateKey
}

// SetupSuite parses the anonymized PKCS#8 test private key once for every test in the suite (same
// key SignerSuite uses, so both suites exercise the identical signing material).
func (s *TransactionsSuite) SetupSuite() {
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
func (s *TransactionsSuite) newSigner() *enablebanking.Signer {
	now := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)

	return enablebanking.NewSigner(testAppID, s.key, func() time.Time { return now })
}

// fixture reads one anonymized testdata file, failing the test immediately if it is missing. It is
// always called from the suite's own goroutine (never from inside an httptest.Server handler,
// where a fatal assertion would be unsafe), so every response body a test needs is read up front
// and only plain byte slices are captured by handler closures.
func (s *TransactionsSuite) fixture(name string) []byte {
	s.T().Helper()

	path := filepath.Clean(filepath.Join(fixtureDir, name))

	data, err := os.ReadFile(path)
	s.Require().NoError(err, "read fixture %s", path)

	return data
}

// testAccount builds a bank.Account for uid, with the rest of the fields fixed, anonymized
// placeholders (constitution: no real IBAN, name or identifier in a tracked file).
func testAccount(uid string) bank.Account {
	return bank.Account{
		BankKey:  "test-bank",
		UID:      uid,
		Hash:     "00000000000000000000000000000001",
		IBAN:     "LT000000000000000001",
		Currency: "EUR",
		Name:     "Test Account",
	}
}

// writeJSON writes body as a status JSON response, the shape every Enable Banking response in
// this suite takes (a success page or an ErrorResponse body).
func writeJSON(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// fixedResponseRoundTripper returns the same canned status and Retry-After header for every
// request, never touching a socket. It exists to drive httpx.RetryTransport's retry-cap decision
// (an oversized Retry-After fails the request immediately, with no wait) without an
// httptest.Server and without a real sleep.
type fixedResponseRoundTripper struct {
	status     int
	retryAfter string
}

// RoundTrip implements http.RoundTripper.
func (rt *fixedResponseRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	header := make(http.Header)
	if rt.retryAfter != "" {
		header.Set("Retry-After", rt.retryAfter)
	}

	return &http.Response{
		StatusCode: rt.status,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader("")),
		Request:    req,
	}, nil
}

// TestClientSatisfiesBankProviderInterface is a compile-time check that *enablebanking.Client
// implements bank.Provider, per the T029 ruling on the constructor shape.
func (s *TransactionsSuite) TestClientSatisfiesBankProviderInterface() {
	var _ bank.Provider = (*enablebanking.Client)(nil)
}

// TestTransactionsRequestHasDateFromOnlyBearerAuthAndNoPsuHeaders asserts the request shape bullet
// of the T029 design: GET /accounts/{uid}/transactions with date_from = from-1day, no date_to and
// no transaction_status, an Authorization: Bearer <jwt> header, and never a header starting with
// Psu- (account-information consent only, constitution §II).
func (s *TransactionsSuite) TestTransactionsRequestHasDateFromOnlyBearerAuthAndNoPsuHeaders() {
	acc := testAccount("uid-100")
	from := civil.Date{Year: 2026, Month: time.September, Day: 5}
	signer := s.newSigner()

	wantToken, err := signer.Token()
	s.Require().NoError(err)

	body := s.fixture("tx_flat_p3.json")

	var (
		gotMethod string
		gotPath   string
		gotQuery  url.Values
		gotHeader http.Header
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.Query()
		gotHeader = r.Header.Clone()

		writeJSON(w, http.StatusOK, body)
	}))
	defer server.Close()

	client := enablebanking.New(server.URL, &http.Client{}, signer, redact.New())

	_, err = client.Transactions(context.Background(), "session-1", acc, from)
	s.Require().NoError(err)

	s.Equal(http.MethodGet, gotMethod)
	s.Equal("/accounts/uid-100/transactions", gotPath)

	s.Equal("2026-09-04", gotQuery.Get("date_from"), "date_from must be from minus one day (R5)")
	s.False(gotQuery.Has("date_to"), "date_to must never be sent (R5)")
	s.False(gotQuery.Has("transaction_status"), "transaction_status must never be sent (R5)")

	s.Equal("Bearer "+wantToken, gotHeader.Get("Authorization"))

	for name := range gotHeader {
		s.Falsef(
			strings.HasPrefix(name, "Psu-"),
			"unexpected consent-only-violating header %s (constitution §II)",
			name,
		)
	}
}

// TestTransactionsMapsFlatPagesEveryEntryAndFollowsPagination asserts, in one pass over
// tx_flat_p1.json -> tx_flat_p2_empty.json -> tx_flat_p3.json, that pagination follows
// continuation_key through the empty middle page (k2 -> k3 -> null), and that every one of the 13
// resulting entries has the exact status (R5 mapping table), date (FR-005a precedence), amount
// (sign rules) and description (precedence + control-character sanitization) the design demands.
// The client does not deduplicate ER-1's PDNG/BOOK pair itself (R6 dedup happens in the reconcile
// package, not here), so both copies must come back.
func (s *TransactionsSuite) TestTransactionsMapsFlatPagesEveryEntryAndFollowsPagination() {
	acc := testAccount("uid-200")
	from := civil.Date{Year: 2026, Month: time.September, Day: 1}
	signer := s.newSigner()

	p1 := s.fixture("tx_flat_p1.json")
	p2Empty := s.fixture("tx_flat_p2_empty.json")
	p3 := s.fixture("tx_flat_p3.json")
	terminal := []byte(`{"transactions":[],"continuation_key":null}`)

	var seenKeys []string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Query().Get("continuation_key")
		seenKeys = append(seenKeys, key)

		switch key {
		case "":
			writeJSON(w, http.StatusOK, p1)
		case "k2":
			writeJSON(w, http.StatusOK, p2Empty)
		case "k3":
			writeJSON(w, http.StatusOK, p3)
		default:
			// Safety net only: an unexpected key must never hang the client in a retry loop.
			// The seenKeys assertion below is what actually fails the test in that case.
			writeJSON(w, http.StatusOK, terminal)
		}
	}))
	defer server.Close()

	client := enablebanking.New(server.URL, &http.Client{}, signer, redact.New())

	got, err := client.Transactions(context.Background(), "session-1", acc, from)
	s.Require().NoError(err)

	s.Equal([]string{"", "k2", "k3"}, seenKeys, "must follow continuation_key through the empty middle page")

	type want struct {
		ref    string
		status bank.Status
		date   civil.Date
		amount string
		desc   string
	}

	wants := []want{
		{"ER-1", bank.Booked, civil.Date{Year: 2026, Month: time.September, Day: 5}, "-4.50", "Coffee Shop"},
		{"ER-1", bank.Pending, civil.Date{Year: 2026, Month: time.September, Day: 5}, "-4.50", "Coffee Shop"},
		{"ER-2", bank.Pending, civil.Date{Year: 2026, Month: time.September, Day: 6}, "15.00", "Employer Ltd"},
		{"ER-3", bank.Pending, civil.Date{Year: 2026, Month: time.September, Day: 7}, "-3.20", "Grocery Store"},
		{"ER-4", bank.Void, civil.Date{Year: 2026, Month: time.September, Day: 8}, "-50.00", "Grocery Store"},
		{"ER-5", bank.Void, civil.Date{Year: 2026, Month: time.September, Day: 9}, "-12.00", "Coffee Shop"},
		{"ER-6", bank.Void, civil.Date{Year: 2026, Month: time.September, Day: 20}, "-100.00", "Employer Ltd"},
		{"ER-7", bank.Void, civil.Date{Year: 2026, Month: time.September, Day: 10}, "0.00", "Coffee Shop"},
		{"ER-8", bank.Booked, civil.Date{Year: 2026, Month: time.September, Day: 11}, "-6.00", "Grocery Store"},
		{"ER-9", bank.Booked, civil.Date{Year: 2026, Month: time.September, Day: 12}, "7.00", "Employer Ltd"},
		{"ER-10", bank.Booked, civil.Date{Year: 2026, Month: time.September, Day: 13}, "-7.00", "ControlChar payment"},
		{"ER-11", bank.Booked, civil.Date{Year: 2026, Month: time.September, Day: 14}, "-9.99", "Grocery Store"},
		{"ER-12", bank.Booked, civil.Date{Year: 2026, Month: time.September, Day: 17}, "-3.30", "(no description)"},
	}

	s.Require().Len(got, len(wants))

	for i, w := range wants {
		s.Run(fmt.Sprintf("%s_%d", w.ref, i), func() {
			tx := got[i]

			s.Equal(w.ref, tx.EntryRef)
			s.Equal(w.status, tx.Status)
			s.Equal(w.date, tx.Date)

			wantAmount, err := domain.ParseAmount(w.amount, "EUR")
			s.Require().NoError(err)
			s.True(
				tx.Amount.Equal(wantAmount),
				"entry %s: got amount %s, want %s",
				w.ref,
				tx.Amount.Format(2),
				w.amount,
			)

			s.Equal(w.desc, tx.Description)
			s.Equal(acc, tx.Account)
		})
	}
}

// TestTransactionsPaginationCapAt100PagesReturnsErrDataIncomplete asserts the pagination safety
// cap (research R5): an account whose continuation_key is never null or absent is unchecked with
// bank.ErrDataIncomplete after exactly 100 pages, instead of looping forever.
func (s *TransactionsSuite) TestTransactionsPaginationCapAt100PagesReturnsErrDataIncomplete() {
	acc := testAccount("uid-300")
	from := civil.Date{Year: 2026, Month: time.September, Day: 1}
	signer := s.newSigner()

	page := []byte(`{"transactions":[],"continuation_key":"next"}`)

	var calls atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeJSON(w, http.StatusOK, page)
	}))
	defer server.Close()

	client := enablebanking.New(server.URL, &http.Client{}, signer, redact.New())

	got, err := client.Transactions(context.Background(), "session-1", acc, from)

	s.Require().Error(err)
	s.Require().ErrorIs(err, bank.ErrDataIncomplete)
	s.Empty(got)
	s.Equal(int32(paginationCap), calls.Load(), "must give up after exactly the 100-page cap (research R5)")
}

// TestTransactionsParsesGroupedResponseShape asserts the grouped
// {"transactions":{"booked":[...],"pending":[...]}} shape (research R5) parses the same as the
// flat shape, with the group itself implying BOOK or PDNG.
func (s *TransactionsSuite) TestTransactionsParsesGroupedResponseShape() {
	acc := testAccount("uid-400")
	from := civil.Date{Year: 2026, Month: time.September, Day: 1}
	signer := s.newSigner()

	body := s.fixture("tx_grouped.json")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, body)
	}))
	defer server.Close()

	client := enablebanking.New(server.URL, &http.Client{}, signer, redact.New())

	got, err := client.Transactions(context.Background(), "session-1", acc, from)
	s.Require().NoError(err)
	s.Require().Len(got, 2)

	booked := got[0]
	s.Equal("ER-20", booked.EntryRef)
	s.Equal(bank.Booked, booked.Status)
	s.Equal(civil.Date{Year: 2026, Month: time.September, Day: 15}, booked.Date)

	wantBookedAmount, err := domain.ParseAmount("-3.30", "EUR")
	s.Require().NoError(err)
	s.True(booked.Amount.Equal(wantBookedAmount))
	s.Equal("Coffee Shop", booked.Description)

	pending := got[1]
	s.Equal("ER-21", pending.EntryRef)
	s.Equal(bank.Pending, pending.Status)
	s.Equal(civil.Date{Year: 2026, Month: time.September, Day: 16}, pending.Date)

	wantPendingAmount, err := domain.ParseAmount("-8.80", "EUR")
	s.Require().NoError(err)
	s.True(pending.Amount.Equal(wantPendingAmount))
	s.Equal("Grocery Store", pending.Description)
}

// TestTransactionsMissingAllDateFieldsReturnsErrDataIncomplete asserts FR-005a: an entry with none
// of transaction_date, booking_date or value_date makes the whole call fail with
// bank.ErrDataIncomplete, never a guessed date.
func (s *TransactionsSuite) TestTransactionsMissingAllDateFieldsReturnsErrDataIncomplete() {
	acc := testAccount("uid-500")
	from := civil.Date{Year: 2026, Month: time.September, Day: 1}
	signer := s.newSigner()

	body := s.fixture("tx_nodate.json")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, body)
	}))
	defer server.Close()

	client := enablebanking.New(server.URL, &http.Client{}, signer, redact.New())

	got, err := client.Transactions(context.Background(), "session-1", acc, from)
	s.Require().Error(err)
	s.Require().ErrorIs(err, bank.ErrDataIncomplete)
	s.Empty(got)
}

// TestTransactionsErrorBodyMapsToSentinel asserts research R7's error-code mapping for the three
// named ASPSP error codes: a rate-limited response, an expired session and both spellings of a
// revoked/closed session.
func (s *TransactionsSuite) TestTransactionsErrorBodyMapsToSentinel() {
	cases := []struct {
		name    string
		fixture string
		status  int
		want    error
	}{
		{"rate limited", "err_rate_limit.json", http.StatusTooManyRequests, bank.ErrRateLimited},
		{"expired session", "err_expired_session.json", http.StatusUnauthorized, bank.ErrConsentExpired},
		{"revoked session", "err_revoked_session.json", http.StatusUnauthorized, bank.ErrConsentRevoked},
		{"closed session", "err_closed_session.json", http.StatusUnauthorized, bank.ErrConsentRevoked},
	}

	for _, tc := range cases {
		s.Run(tc.name, func() {
			acc := testAccount("uid-err")
			from := civil.Date{Year: 2026, Month: time.September, Day: 1}
			signer := s.newSigner()
			body := s.fixture(tc.fixture)

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, tc.status, body)
			}))
			defer server.Close()

			client := enablebanking.New(server.URL, &http.Client{}, signer, redact.New())

			got, err := client.Transactions(context.Background(), "session-1", acc, from)
			s.Require().Error(err)
			s.Require().ErrorIs(err, tc.want)
			s.Empty(got)
		})
	}
}

// TestTransactionsGenericErrorMapsToBankErrorWithRedactedIBAN asserts the fallback of research R7:
// an ASPSP error code outside the three specially-mapped ones still comes back as a *bank.Error,
// and its Detail is redacted before it ever leaves the process (constitution §V) -- proven here
// with a fabricated message that happens to carry an IBAN-shaped substring.
func (s *TransactionsSuite) TestTransactionsGenericErrorMapsToBankErrorWithRedactedIBAN() {
	acc := testAccount("uid-generic-err")
	from := civil.Date{Year: 2026, Month: time.September, Day: 1}
	signer := s.newSigner()

	rawIBAN := "LT121000011101001000"
	body := []byte(
		`{"message":"access denied for account ` + rawIBAN + `","code":403,"error":"ACCESS_DENIED","detail":"none"}`,
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusForbidden, body)
	}))
	defer server.Close()

	redactor := redact.New()
	client := enablebanking.New(server.URL, &http.Client{}, signer, redactor)

	got, err := client.Transactions(context.Background(), "session-1", acc, from)
	s.Require().Error(err)
	s.Empty(got)

	s.Require().NotErrorIs(err, bank.ErrRateLimited)
	s.Require().NotErrorIs(err, bank.ErrConsentExpired)
	s.Require().NotErrorIs(err, bank.ErrConsentRevoked)

	var bankErr *bank.Error
	s.Require().ErrorAs(err, &bankErr)
	s.NotContains(bankErr.Detail, rawIBAN, "the raw IBAN must never leave the process (constitution §V)")
	s.Contains(bankErr.Detail, redactor.Scrub(rawIBAN), "the detail must carry the redactor's masked form")
}

// TestTransactionsRetryAfterTooLongMapsToErrRateLimited asserts the second half of the T029
// ruling: when the caller wires the injected *http.Client with an httpx.RetryTransport (the
// production shape) and a 429 response's Retry-After exceeds the retry cap, http.Client.Do never
// returns a response at all -- it returns a *httpx.RetryAfterTooLongError -- and Transactions must
// still map that to bank.ErrRateLimited, exactly like a final 429 body would. The fake
// RoundTripper never touches a socket, and the oversized Retry-After (120s, over
// RetryTransport's default 60s MaxWait) makes the error return before any wait, so this needs
// neither an httptest.Server nor a real sleep.
func (s *TransactionsSuite) TestTransactionsRetryAfterTooLongMapsToErrRateLimited() {
	acc := testAccount("uid-retry-after")
	from := civil.Date{Year: 2026, Month: time.September, Day: 1}
	signer := s.newSigner()

	hc := httpx.NewClient(&httpx.RetryTransport{
		Base: &fixedResponseRoundTripper{status: http.StatusTooManyRequests, retryAfter: "120"},
	})

	client := enablebanking.New("http://enablebanking.test", hc, signer, redact.New())

	got, err := client.Transactions(context.Background(), "session-1", acc, from)
	s.Require().Error(err)
	s.Require().ErrorIs(err, bank.ErrRateLimited)
	s.Empty(got)
}
