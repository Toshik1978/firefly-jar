package firefly

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/suite"

	"github.com/Toshik1978/firefly-jar/internal/civil"
	"github.com/Toshik1978/firefly-jar/internal/domain"
)

// TransactionsSuite covers (*Client).ListAccountTransactions (T035, FR-006, FR-006a, FR-009,
// research R8): the request shape (start/end as YYYY-MM-DD, types=withdrawal,deposit,transfer,
// type=default, limit=500), the client's own page counter, merging a group's splits across pages
// by group id and transaction_journal_id, filtering to withdrawal/deposit/transfer splits only,
// choosing amount/foreign_amount by currency match, the source/destination sign rule, the
// YYYY-MM-DD date-prefix rule, the 200-page safety cap, and the 404/non-JSON error mappings. It is
// the RED half of T036: every case here fails today for lack of a production
// ListAccountTransactions, ErrNotFound, ErrDataIncomplete, and the unexported withLogger DEBUG-log
// injection point this suite relies on.
//
// Logger-injection decision (T035, binding on T036): Client gains an unexported `logger
// *slog.Logger` field, defaulting in New to slog.New(slog.DiscardHandler), plus an unexported
// `(c *Client) withLogger(logger *slog.Logger) *Client` that returns a shallow copy of c logging to
// logger instead. New's exported signature does not change. The single DEBUG record a skipped,
// incomparable split produces carries exactly four snake_case attrs -- group_id (string),
// account_id (the raw Firefly account id: an internal database number, not a bank identifier, so
// the IBAN masking rule does not apply and masking it would render every id as asterisks), date
// (the split's own YYYY-MM-DD date prefix, never re-converted, per the Entry.Date rule) and amount
// (the split's raw, unparsed `amount` field string, exactly as Firefly sent it, since no
// domain.Amount can be constructed in the account's currency for an incomparable split) -- and
// never the split's description, per constitution §V.
type TransactionsSuite struct {
	suite.Suite
}

// fixture reads one anonymized testdata file from the same fixture directory AccountsSuite uses,
// failing the test immediately if it is missing.
func (s *TransactionsSuite) fixture(name string) []byte {
	s.T().Helper()

	path := filepath.Clean(filepath.Join(accountsFixtureDir, name))

	data, err := os.ReadFile(path)
	s.Require().NoError(err, "read fixture %s", path)

	return data
}

// mustDate parses str as a civil.Date, failing the test immediately on a malformed literal.
func (s *TransactionsSuite) mustDate(str string) civil.Date {
	s.T().Helper()

	d, err := civil.ParseDate(str)
	s.Require().NoError(err)

	return d
}

// mustAmount parses str as a domain.Amount in currency, failing the test immediately on a
// malformed literal.
func (s *TransactionsSuite) mustAmount(str, currency string) domain.Amount {
	s.T().Helper()

	a, err := domain.ParseAmount(str, currency)
	s.Require().NoError(err)

	return a
}

// pagedTxServer starts an httptest.Server that serves bodies in request order, recording every
// request's path and query. Any request past len(bodies) gets an empty, meta-less terminal page, so
// a client that fails to stop at total_pages can never hang the test.
func (s *TransactionsSuite) pagedTxServer(bodies ...[]byte) (*httptest.Server, *[]string, *[]url.Values) {
	var paths []string

	queries := make([]url.Values, 0, len(bodies))
	terminal := []byte(`{"data":[]}`)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		queries = append(queries, r.URL.Query())

		idx := len(queries) - 1
		if idx < len(bodies) {
			writeJSON(w, http.StatusOK, bodies[idx])

			return
		}

		writeJSON(w, http.StatusOK, terminal)
	}))

	return server, &paths, &queries
}

// capPageBody builds a single-split transactions page reporting total_pages far beyond the
// 200-page safety cap and current_page stuck at 1 (mirroring the accounts fixtures' proof that the
// client must count pages itself), so a client that failed to enforce the cap would loop forever
// instead of stopping at exactly 200 requests. Each page carries a unique group and journal id so a
// buggy implementation cannot mistake this for the group-merging case.
func capPageBody(page int) []byte {
	id := fmt.Sprintf("cap-%d", page)

	body := map[string]any{
		"data": []map[string]any{
			{
				"type": "transactions",
				"id":   id,
				"attributes": map[string]any{
					"transactions": []map[string]any{
						{
							"transaction_journal_id": id,
							"type":                   "withdrawal",
							"date":                   "2026-09-01T00:00:00+02:00",
							"currency_code":          "EUR",
							"foreign_currency_code":  nil,
							"amount":                 "1.000000000000",
							"foreign_amount":         nil,
							"description":            "anonymized",
							"source_id":              "1",
							"destination_id":         "9000",
						},
					},
				},
			},
		},
		"meta": map[string]any{
			"pagination": map[string]any{
				"total":        100000,
				"count":        1,
				"per_page":     500,
				"current_page": 1,
				"total_pages":  100000,
			},
		},
	}

	data, err := json.Marshal(body)
	if err != nil {
		panic(err) // unreachable: body is a static, always-marshalable literal.
	}

	return data
}

// TestListAccountTransactionsRequestsExactQueryAndOwnPageCounter asserts the request-shape bullet
// of the T035 design: GET /accounts/{id}/transactions with start/end as YYYY-MM-DD plus
// types=withdrawal,deposit,transfer, type=default and limit=500, and that the client drives page
// itself from 1 up to meta.pagination.total_pages, exactly as ListAccounts must (T034).
func (s *TransactionsSuite) TestListAccountTransactionsRequestsExactQueryAndOwnPageCounter() {
	p1 := s.fixture("acc_tx_p1.json")
	p2 := s.fixture("acc_tx_p2.json")

	server, paths, queries := s.pagedTxServer(p1, p2)
	defer server.Close()

	client := New(server.URL, "token", http.DefaultTransport)

	start := s.mustDate("2026-09-01")
	end := s.mustDate("2026-09-23")

	_, err := client.ListAccountTransactions(context.Background(), "1", "EUR", start, end)
	s.Require().NoError(err)

	s.Require().Len(*queries, 2, "must stop at total_pages using its own counter")

	for _, path := range *paths {
		s.Equal("/accounts/1/transactions", path)
	}

	s.Equal(url.Values{
		"start": {"2026-09-01"},
		"end":   {"2026-09-23"},
		"types": {"withdrawal,deposit,transfer"},
		"type":  {"default"},
		"limit": {"500"},
		"page":  {"1"},
	}, (*queries)[0])

	s.Equal(url.Values{
		"start": {"2026-09-01"},
		"end":   {"2026-09-23"},
		"types": {"withdrawal,deposit,transfer"},
		"type":  {"default"},
		"limit": {"500"},
		"page":  {"2"},
	}, (*queries)[1])
}

// TestListAccountTransactionsMergesFiltersSignsAndSkipsIncomparableSplits asserts almost every
// remaining bullet of the T035 design against the full acc_tx_p1.json/acc_tx_p2.json case table
// (T033's fixture report): group 6 (opening balance), group 8 (reconciliation) and group 9
// (unknown-to-domain "liability credit", which must not fail decoding) never produce an entry;
// group 5's cross-currency transfer picks foreign_amount because foreign_currency_code matches the
// account's EUR; the sign follows source_id/destination_id; group 7's two withdrawal splits, split
// across both pages under the same group id but different transaction_journal_id, merge into one
// entry whose amount is their signed sum and whose date is the *first* split's date prefix; and
// group 10's USD withdrawal, with no foreign_amount, is skipped and produces exactly one DEBUG log
// record carrying only the group id, raw Firefly account id, date and amount -- never the
// description.
func (s *TransactionsSuite) TestListAccountTransactionsMergesFiltersSignsAndSkipsIncomparableSplits() {
	p1 := s.fixture("acc_tx_p1.json")
	p2 := s.fixture("acc_tx_p2.json")

	server, _, _ := s.pagedTxServer(p1, p2)
	defer server.Close()

	var logBuf bytes.Buffer

	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	client := New(server.URL, "token", http.DefaultTransport).withLogger(logger)

	got, err := client.ListAccountTransactions(
		context.Background(), "1", "EUR", s.mustDate("2026-09-01"), s.mustDate("2026-09-23"),
	)
	s.Require().NoError(err)

	want := []Entry{
		{
			GroupID: "1", AccountID: "1", Date: s.mustDate("2026-09-20"),
			Amount: s.mustAmount("-12.34", "EUR"), Description: "anonymized",
		},
		{
			GroupID: "2", AccountID: "1", Date: s.mustDate("2026-09-10"),
			Amount: s.mustAmount("1500.50", "EUR"), Description: "anonymized",
		},
		{
			GroupID: "3", AccountID: "1", Date: s.mustDate("2026-09-05"),
			Amount: s.mustAmount("-200.00", "EUR"), Description: "anonymized",
		},
		{
			GroupID: "4", AccountID: "1", Date: s.mustDate("2026-09-06"),
			Amount: s.mustAmount("50.00", "EUR"), Description: "anonymized",
		},
		{
			GroupID: "5", AccountID: "1", Date: s.mustDate("2026-09-07"),
			Amount: s.mustAmount("92.50", "EUR"), Description: "anonymized",
		},
		{
			GroupID: "7", AccountID: "1", Date: s.mustDate("2026-09-18"),
			Amount: s.mustAmount("-40.00", "EUR"), Description: "anonymized",
		},
	}

	s.Require().Len(
		got, len(want),
		"opening balance, reconciliation, liability credit and the incomparable USD withdrawal "+
			"(group 10) must never produce an entry",
	)

	for i, w := range want {
		s.Run("group_"+w.GroupID, func() {
			s.Equal(w.GroupID, got[i].GroupID)
			s.Equal(w.AccountID, got[i].AccountID)
			s.Equal(w.Date, got[i].Date)
			s.True(
				w.Amount.Equal(got[i].Amount),
				"amount for group %s: want %s got %s", w.GroupID, w.Amount.Value, got[i].Amount.Value,
			)
			s.Equal(w.Description, got[i].Description)
		})
	}

	lines := bytes.Split(bytes.TrimSpace(logBuf.Bytes()), []byte("\n"))
	s.Require().Len(lines, 1, "exactly one split (group 10) must be skipped as incomparable")

	var rec map[string]any
	s.Require().NoError(json.Unmarshal(lines[0], &rec))

	s.Equal("DEBUG", rec["level"])
	s.Equal("10", rec["group_id"])
	s.Equal("1", rec["account_id"], "the raw Firefly account id, not a bank identifier")
	s.NotContains(rec, "account", "the account is identified by account_id only")
	s.Equal("2026-09-04", rec["date"])
	s.Equal("45.000000000000", rec["amount"])

	s.NotContains(logBuf.String(), "anonymized", "the description must never be logged (constitution §V)")
	s.NotContains(logBuf.String(), "description")
}

// TestListAccountTransactionsCountsSplitsWithoutJournalID asserts that deduplication by
// transaction_journal_id only applies to splits that carry one: two splits of the same group with
// no transaction_journal_id are distinct splits, and both must be counted into the group's sum
// rather than the second being dropped silently as a "duplicate" of the empty id.
func (s *TransactionsSuite) TestListAccountTransactionsCountsSplitsWithoutJournalID() {
	body := []byte(`{"data":[{"type":"transactions","id":"42","attributes":{"group_title":null,"transactions":[
		{"type":"withdrawal","date":"2026-09-12T10:00:00+02:00","currency_code":"EUR",
		 "amount":"10.000000000000","description":"anonymized","source_id":"1","destination_id":"3000"},
		{"type":"withdrawal","date":"2026-09-12T10:00:00+02:00","currency_code":"EUR",
		 "amount":"5.000000000000","description":"anonymized","source_id":"1","destination_id":"3000"}
	]}}]}`)

	server, _, _ := s.pagedTxServer(body)
	defer server.Close()

	client := New(server.URL, "token", http.DefaultTransport)

	got, err := client.ListAccountTransactions(
		context.Background(), "1", "EUR", s.mustDate("2026-09-01"), s.mustDate("2026-09-23"),
	)
	s.Require().NoError(err)
	s.Require().Len(got, 1)

	s.Equal("42", got[0].GroupID)
	s.True(
		s.mustAmount("-15.00", "EUR").Equal(got[0].Amount),
		"both splits without a journal id must be summed, got %s", got[0].Amount.Value,
	)
}

// TestListAccountTransactionsPageCapReturnsErrDataIncomplete asserts the 200-page safety-cap bullet:
// hitting the cap must return an error satisfying errors.Is(err, ErrDataIncomplete), never a silent
// truncation, and the client must give up after exactly 200 requests, not 199 or 201.
func (s *TransactionsSuite) TestListAccountTransactionsPageCapReturnsErrDataIncomplete() {
	var hits int

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		writeJSON(w, http.StatusOK, capPageBody(hits))
	}))
	defer server.Close()

	client := New(server.URL, "token", http.DefaultTransport)

	got, err := client.ListAccountTransactions(
		context.Background(), "1", "EUR", s.mustDate("2026-09-01"), s.mustDate("2026-09-23"),
	)
	s.Require().Error(err)
	s.Empty(got)

	s.Require().ErrorIs(err, ErrDataIncomplete)
	s.Equal(200, hits, "must give up after exactly the 200-page safety cap")
}

// TestListAccountTransactionsNotFoundWrapsErrNotFound asserts the 404 bullet: a 404 response maps to
// an error satisfying errors.Is(err, ErrNotFound), whose message names the requested account
// (research R8: "404 means account #N not found").
func (s *TransactionsSuite) TestListAccountTransactionsNotFoundWrapsErrNotFound() {
	body := s.fixture("err_404.json")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotFound, body)
	}))
	defer server.Close()

	client := New(server.URL, "token", http.DefaultTransport)

	got, err := client.ListAccountTransactions(
		context.Background(), "1", "EUR", s.mustDate("2026-09-01"), s.mustDate("2026-09-23"),
	)
	s.Require().Error(err)
	s.Empty(got)

	s.Require().ErrorIs(err, ErrNotFound)
	s.Contains(err.Error(), "account #1 not found")
}

// TestListAccountTransactionsNonJSONErrorBodyReportsStatusCodeOnly asserts the T035 ruling: a
// non-JSON error body must yield an error carrying only the status code, never any part of the raw
// body, and it must not satisfy any of the specific sentinels since it is neither a 401 nor a 404.
//
// A 500 is retried by the httpx.RetryTransport New wires in (1s, 2s, 4s backoff), so the call runs
// inside a testing/synctest bubble against htmlErrorTransport, an in-process fake base transport:
// the backoff elapses on the bubble's fake clock instead of real sleeps, and no socket or
// httptest.Server is used inside the bubble (.claude/CLAUDE.md). Results are collected inside the
// bubble and asserted with suite methods after synctest.Test returns, since the bubble's own
// *testing.T is not s.T().
func (s *TransactionsSuite) TestListAccountTransactionsNonJSONErrorBodyReportsStatusCodeOnly() {
	start := s.mustDate("2026-09-01")
	end := s.mustDate("2026-09-23")
	fake := &htmlErrorTransport{
		body: "<html>Internal Server Error, node: db-primary-07, trace 8f21ac</html>",
	}

	var (
		got []Entry
		err error
	)

	synctest.Test(s.T(), func(t *testing.T) {
		client := New("https://firefly.example.com/api/v1", "token", fake)

		got, err = client.ListAccountTransactions(t.Context(), "1", "EUR", start, end)
	})

	s.Equal(int32(4), fake.attempts.Load(), "one attempt plus the retry transport's three retries")

	s.Require().Error(err)
	s.Empty(got)

	s.Require().NotErrorIs(err, ErrNotFound)
	s.Require().NotErrorIs(err, ErrUnauthorized)
	s.Require().NotErrorIs(err, ErrDataIncomplete)

	s.Contains(err.Error(), "500", "a non-JSON error body must be reported by status code only")
	s.NotContains(err.Error(), "db-primary-07", "the raw error body must never reach the error message")
	s.NotContains(err.Error(), "Internal Server Error", "the raw error body must never reach the error message")
}

// htmlErrorTransport is an in-process, synctest-safe base transport that answers every request
// with a 500 and a non-JSON HTML body, counting the attempts that reach it.
type htmlErrorTransport struct {
	body     string
	attempts atomic.Int32
}

func (f *htmlErrorTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	f.attempts.Add(1)

	return &http.Response{
		Status:     "500 Internal Server Error",
		StatusCode: http.StatusInternalServerError,
		Header:     http.Header{"Content-Type": {"text/html"}},
		Body:       io.NopCloser(strings.NewReader(f.body)),
		Request:    req,
	}, nil
}
