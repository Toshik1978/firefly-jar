package firefly

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"

	"github.com/jarcoal/httpmock"
	"github.com/stretchr/testify/suite"
)

// accountsFixtureDir holds the anonymized Firefly III account list fixtures T033 built (relative to
// this package directory).
const accountsFixtureDir = "../../testdata/firefly"

// AccountsSuite covers (*Client).ListAccounts (T034, FR-001, research R8): the request shape
// (type=asset&limit=500&page=N), pagination driven by the client's own page counter rather than the
// server's reported current_page, the missing-meta single-page case, the exact field mapping from
// the Firefly III JSON:API shape into Account, and the 401 -> ErrUnauthorized error mapping. It is
// the RED half of T036: every case here fails today for lack of a production ListAccounts,
// ErrUnauthorized, and the models/errors files T036 adds.
type AccountsSuite struct {
	suite.Suite
}

// fixture reads one anonymized testdata file, failing the test immediately if it is missing.
func (s *AccountsSuite) fixture(name string) []byte {
	s.T().Helper()

	path := filepath.Clean(filepath.Join(accountsFixtureDir, name))

	data, err := os.ReadFile(path)
	s.Require().NoError(err, "read fixture %s", path)

	return data
}

// pagedAccountsTransport returns an httpmock.MockTransport that answers GET /accounts by serving
// bodies in order on successive requests, recording each request's query so a test can assert the
// exact query per call. Any request past len(bodies) -- which would only happen if ListAccounts
// fails to stop at total_pages -- gets accountsFixtureNoMeta, a safe single-page terminal that can
// never cause a hang, instead of repeating the last real page forever.
func (s *AccountsSuite) pagedAccountsTransport(bodies ...[]byte) (*httpmock.MockTransport, *[]url.Values) {
	queries := make([]url.Values, 0, len(bodies))
	terminal := s.fixture("accounts_nometa.json")

	transport := httpmock.NewMockTransport()
	transport.RegisterResponder(http.MethodGet, fireflyTestURL+"/accounts",
		func(req *http.Request) (*http.Response, error) {
			queries = append(queries, req.URL.Query())

			idx := len(queries) - 1
			if idx < len(bodies) {
				return httpmock.NewBytesResponse(http.StatusOK, bodies[idx]), nil
			}

			return httpmock.NewBytesResponse(http.StatusOK, terminal), nil
		},
	)

	return transport, &queries
}

// TestListAccountsRequestsAssetTypeLimit500AndOwnPageCounter asserts the request shape bullet of the
// T034 design -- GET /accounts?type=asset&limit=500&page=N -- and that ListAccounts drives page
// itself from 1 up to meta.pagination.total_pages: accounts_p1.json and accounts_p2.json both report
// current_page: 1 and total_pages: 2, so a client that trusted current_page would never advance, or
// would loop forever. Exactly two requests must be made, with page=1 then page=2.
func (s *AccountsSuite) TestListAccountsRequestsAssetTypeLimit500AndOwnPageCounter() {
	p1 := s.fixture("accounts_p1.json")
	p2 := s.fixture("accounts_p2.json")

	transport, queries := s.pagedAccountsTransport(p1, p2)

	client := New(fireflyTestURL, "token", transport)

	got, err := client.ListAccounts(context.Background())
	s.Require().NoError(err)
	s.Len(got, 7, "must merge both pages into one account list")

	s.Require().Len(
		*queries,
		2,
		"must stop at total_pages using its own counter, even though current_page is stuck at 1",
	)

	s.Equal(url.Values{"type": {"asset"}, "limit": {"500"}, "page": {"1"}}, (*queries)[0])
	s.Equal(url.Values{"type": {"asset"}, "limit": {"500"}, "page": {"2"}}, (*queries)[1])
}

// TestListAccountsMissingMetaFetchesExactlyOnePage asserts the "missing meta means a single page"
// bullet of the T034 design: accounts_nometa.json has a data array but no meta key at all, and
// ListAccounts must not attempt a second page.
func (s *AccountsSuite) TestListAccountsMissingMetaFetchesExactlyOnePage() {
	body := s.fixture("accounts_nometa.json")

	transport := httpmock.NewMockTransport()
	transport.RegisterResponder(http.MethodGet, fireflyTestURL+"/accounts",
		httpmock.NewBytesResponder(http.StatusOK, body),
	)

	client := New(fireflyTestURL, "token", transport)

	got, err := client.ListAccounts(context.Background())
	s.Require().NoError(err)
	s.Require().Len(got, 1)
	s.Equal("1", got[0].ID)

	s.Equal(
		1,
		transport.GetTotalCallCount(),
		"a missing meta.pagination must mean a single page, never a second request",
	)
}

// TestListAccountsMapsEveryFieldExactly asserts the mapping bullet of the T034 design over every
// account in accounts_p1.json and accounts_p2.json (T033's case table): IBAN is uppercased and a
// null IBAN becomes ""; active defaults to true when the key is missing entirely (account "5") and
// stays false when explicit (account "4"); currency_code, currency_decimal_places and account_role
// map to Currency, DecimalPlaces and Role; id and name map directly.
func (s *AccountsSuite) TestListAccountsMapsEveryFieldExactly() {
	p1 := s.fixture("accounts_p1.json")
	p2 := s.fixture("accounts_p2.json")

	transport, _ := s.pagedAccountsTransport(p1, p2)

	client := New(fireflyTestURL, "token", transport)

	got, err := client.ListAccounts(context.Background())
	s.Require().NoError(err)
	s.Require().Len(got, 7)

	wants := []Account{
		{
			ID: "1", Name: "Main Account", IBAN: "LT000000000000000001",
			Currency: "EUR", Role: "defaultAsset", DecimalPlaces: 2, Active: true,
		},
		{
			ID: "2", Name: "Savings", IBAN: "LT000000000000000002",
			Currency: "EUR", Role: "savingAsset", DecimalPlaces: 2, Active: true,
		},
		{
			ID: "3", Name: "Credit Card", IBAN: "",
			Currency: "EUR", Role: "ccAsset", DecimalPlaces: 2, Active: true,
		},
		{
			ID: "4", Name: "Old Account", IBAN: "",
			Currency: "EUR", Role: "defaultAsset", DecimalPlaces: 2, Active: false,
		},
		{
			ID: "5", Name: "No Active Key Account", IBAN: "",
			Currency: "EUR", Role: "defaultAsset", DecimalPlaces: 2, Active: true,
		},
		{
			ID: "6", Name: "Shared IBAN Account (EUR)", IBAN: "LT000000000000000099",
			Currency: "EUR", Role: "defaultAsset", DecimalPlaces: 2, Active: true,
		},
		{
			ID: "7", Name: "Shared IBAN Account (USD)", IBAN: "LT000000000000000099",
			Currency: "USD", Role: "defaultAsset", DecimalPlaces: 2, Active: true,
		},
	}

	for i, want := range wants {
		s.Run("account_"+want.ID, func() {
			s.Equal(want, got[i])
		})
	}
}

// TestListAccountsUnauthorizedWrapsErrUnauthorized asserts the error-mapping bullet of the T034
// design: a 401 response maps to an error satisfying errors.Is(err, ErrUnauthorized), whose message
// contains "Firefly token rejected" (the T034 ruling).
func (s *AccountsSuite) TestListAccountsUnauthorizedWrapsErrUnauthorized() {
	body := s.fixture("err_401.json")

	transport := httpmock.NewMockTransport()
	transport.RegisterResponder(http.MethodGet, fireflyTestURL+"/accounts",
		httpmock.NewBytesResponder(http.StatusUnauthorized, body),
	)

	client := New(fireflyTestURL, "token", transport)

	got, err := client.ListAccounts(context.Background())
	s.Require().Error(err)
	s.Empty(got)

	s.Require().ErrorIs(err, ErrUnauthorized)
	s.Contains(err.Error(), "Firefly token rejected")
}

// TestListAccountsForbiddenWrapsErrUnauthorized asserts a 403 is the same token problem as a 401
// (a token Firefly III, or a proxy in front of it, refuses), so the run reports it as unauthorized
// rather than as an unreachable server.
func (s *AccountsSuite) TestListAccountsForbiddenWrapsErrUnauthorized() {
	transport := httpmock.NewMockTransport()
	transport.RegisterResponder(http.MethodGet, fireflyTestURL+"/accounts",
		httpmock.NewStringResponder(http.StatusForbidden, `{"message":"Forbidden."}`),
	)

	client := New(fireflyTestURL, "token", transport)

	_, err := client.ListAccounts(context.Background())

	s.Require().ErrorIs(err, ErrUnauthorized)
	s.Contains(err.Error(), "status 403")
}
