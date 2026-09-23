package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/stretchr/testify/suite"

	"github.com/Toshik1978/firefly-jar/internal/app"
	"github.com/Toshik1978/firefly-jar/internal/bank"
	"github.com/Toshik1978/firefly-jar/internal/civil"
	"github.com/Toshik1978/firefly-jar/internal/config"
	"github.com/Toshik1978/firefly-jar/internal/digest"
	"github.com/Toshik1978/firefly-jar/internal/domain"
	"github.com/Toshik1978/firefly-jar/internal/firefly"
	"github.com/Toshik1978/firefly-jar/internal/logging"
	"github.com/Toshik1978/firefly-jar/internal/notify"
	"github.com/Toshik1978/firefly-jar/internal/redact"
	"github.com/Toshik1978/firefly-jar/internal/report"
	"github.com/Toshik1978/firefly-jar/internal/state"
)

// Anonymized identifiers of the one configured bank, its session and its accounts. The mapped
// account's IBAN and currency equal those of Firefly III account #1 in accounts_nometa.json, so it
// maps automatically; the unmapped account's IBAN matches no Firefly III account at all.
const (
	checkBankKey        = "testbank"
	checkSessionID      = "00000000-0000-0000-0000-0000000000aa"
	checkAccountUID     = "00000000-0000-0000-0000-000000000001"
	checkAccountHash    = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=.AAAA"
	checkAccountIBAN    = "LT000000000000000001"
	unmappedAccountUID  = "00000000-0000-0000-0000-000000000002"
	unmappedAccountIBAN = "LT000000000000000077"
	checkFireflyID      = "1"
	checkFireflyToken   = "test-firefly-token-0000"
	checkAccountsFile   = "../../testdata/firefly/accounts_nometa.json"

	// checkHeading is the digest's account heading for the mapped account: the display name comes
	// from Config.Banks, never from the bank key, and the IBAN is masked.
	checkHeading = "Testbank · LT00…0001 · Main (EUR)"

	// summaryKey identifies the run summary record among the file log's records.
	summaryKey = "accounts_checked"
)

// ffEntry is one single-split Firefly III transaction group on account #1, as the fake Firefly III
// serves it. Kind is withdrawal (account #1 is the source) or deposit (account #1 is the
// destination); Amount is positive, as Firefly III always renders it.
type ffEntry struct {
	group       string
	kind        string
	date        string
	amount      string
	description string
}

// txQuery is one GET /accounts/{id}/transactions request the fake Firefly III received.
type txQuery struct {
	accountID string
	start     string
	end       string
}

// providerCall is one bank.Provider.Transactions call the fake provider received.
type providerCall struct {
	sessionID string
	account   bank.Account
	from      civil.Date
}

// eventLog records, in order, every outbound call a check makes to the bank or to Firefly III, so
// a case can assert which one came first.
type eventLog struct {
	mu     sync.Mutex
	events []string
}

// add appends one event.
func (l *eventLog) add(event string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.events = append(l.events, event)
}

// all returns a copy of every event so far.
func (l *eventLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return append([]string(nil), l.events...)
}

// fakeProvider is a bank.Provider that returns scripted transactions per account UID, stamped with
// the account it was asked for, as a real provider does, and records every call.
type fakeProvider struct {
	mu     sync.Mutex
	events *eventLog
	txs    map[string][]bank.Transaction
	calls  []providerCall
}

// Transactions records the call and returns the account's scripted transactions.
func (p *fakeProvider) Transactions(
	_ context.Context, sessionID string, acc bank.Account, from civil.Date,
) ([]bank.Transaction, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.calls = append(p.calls, providerCall{sessionID: sessionID, account: acc, from: from})
	p.events.add("bank " + acc.UID)

	out := append([]bank.Transaction(nil), p.txs[acc.UID]...)
	for i := range out {
		out[i].Account = acc
	}

	return out, nil
}

// recorded returns a copy of every call so far.
func (p *fakeProvider) recorded() []providerCall {
	p.mu.Lock()
	defer p.mu.Unlock()

	return append([]providerCall(nil), p.calls...)
}

// recordingNotifier is a notify.Notifier that delivers successfully to one recipient and records
// every digest it was asked to send.
type recordingNotifier struct {
	digests []digest.Digest
}

// Name returns the fake channel name.
func (*recordingNotifier) Name() string {
	return "telegram"
}

// Send records d and reports one successful recipient.
func (n *recordingNotifier) Send(_ context.Context, d digest.Digest) []notify.Result {
	n.digests = append(n.digests, d)

	return []notify.Result{{Recipient: "100000001"}}
}

// fakeFirefly is an httptest Firefly III serving the anonymized accounts fixture and, for account
// #1, transaction groups built from the case's ffEntry values. It records every transactions query.
type fakeFirefly struct {
	srv            *httptest.Server
	events         *eventLog
	accountsBody   []byte
	accountsStatus int
	entries        []ffEntry

	mu      sync.Mutex
	queries []txQuery
}

// recorded returns a copy of every transactions query so far.
func (f *fakeFirefly) recorded() []txQuery {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]txQuery(nil), f.queries...)
}

// serveAccounts serves GET /accounts: the fixture, or accountsStatus when a case sets one.
func (f *fakeFirefly) serveAccounts(w http.ResponseWriter, _ *http.Request) {
	f.events.add("firefly accounts")

	w.Header().Set("Content-Type", "application/json")

	if f.accountsStatus != 0 {
		w.WriteHeader(f.accountsStatus)
		_, _ = w.Write([]byte(`{"message":"Unauthenticated."}`))

		return
	}

	_, _ = w.Write(f.accountsBody)
}

// serveTransactions serves GET /accounts/{id}/transactions as one page (no meta block).
func (f *fakeFirefly) serveTransactions(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	f.events.add("firefly transactions " + id)

	f.mu.Lock()
	f.queries = append(f.queries, txQuery{
		accountID: id,
		start:     r.URL.Query().Get("start"),
		end:       r.URL.Query().Get("end"),
	})
	f.mu.Unlock()

	var entries []ffEntry
	if id == checkFireflyID {
		entries = f.entries
	}

	body, err := json.Marshal(groupsPage(id, entries))
	if err != nil {
		http.Error(w, "encode", http.StatusInternalServerError)

		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

// groupsPage renders entries as one Firefly III transactions list page relative to accountID.
func groupsPage(accountID string, entries []ffEntry) map[string]any {
	data := make([]map[string]any, 0, len(entries))

	for _, e := range entries {
		src, dst := accountID, "900"
		if e.kind == "deposit" {
			src, dst = "901", accountID
		}

		data = append(data, map[string]any{
			"type": "transactions",
			"id":   e.group,
			"attributes": map[string]any{
				"group_title": nil,
				"transactions": []map[string]any{{
					"transaction_journal_id": e.group + "0",
					"type":                   e.kind,
					"date":                   e.date + "T12:00:00+03:00",
					"currency_code":          "EUR",
					"foreign_currency_code":  nil,
					"amount":                 e.amount,
					"foreign_amount":         nil,
					"description":            e.description,
					"source_id":              src,
					"destination_id":         dst,
				}},
			},
		})
	}

	return map[string]any{"data": data}
}

// checkHarness is one case's wiring: config and state built in code, the fakes, the three output
// buffers and the injected clock with its call counter.
type checkHarness struct {
	cfg      *config.Config
	st       *state.State
	events   *eventLog
	provider *fakeProvider
	firefly  *fakeFirefly
	notifier *recordingNotifier
	fileLog  *bytes.Buffer
	stderr   *bytes.Buffer
	stdout   *bytes.Buffer
	clock    time.Time
	nowCalls int
}

// run executes one check with fresh Deps over the harness.
func (h *checkHarness) run(ctx context.Context, opts app.CheckOptions) (report.RunReport, int) {
	return app.Check(ctx, h.deps(), opts)
}

// deps wires the harness into app.Deps. The file log is at DEBUG, so the no-description rule is
// checked against every record the run could write.
func (h *checkHarness) deps() app.Deps {
	redactor := redact.New(checkFireflyToken)

	return app.Deps{
		Config:    h.cfg,
		State:     h.st,
		Provider:  h.provider,
		Firefly:   firefly.New(h.firefly.srv.URL+"/api/v1", checkFireflyToken, h.firefly.srv.Client().Transport),
		Notifiers: []notify.Notifier{h.notifier},
		Redactor:  redactor,
		Log:       logging.New(h.fileLog, h.stderr, slog.LevelDebug, redactor),
		Now: func() time.Time {
			h.nowCalls++

			return h.clock
		},
		Stdout: h.stdout,
	}
}

// addUnmappedAccount adds a second account to the bank's session whose IBAN matches no Firefly III
// account, scripting txs for it.
func (h *checkHarness) addUnmappedAccount(txs []bank.Transaction) {
	session := h.st.Sessions[checkBankKey]
	session.Accounts = append(session.Accounts, state.Account{
		UID:      unmappedAccountUID,
		Hash:     "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB=.BBBB",
		IBAN:     unmappedAccountIBAN,
		Currency: "EUR",
		Name:     "Spare",
	})
	h.st.Sessions[checkBankKey] = session
	h.provider.txs[unmappedAccountUID] = txs
}

// CheckSuite covers app.Check end to end (T051, FR-003, FR-004, FR-006, FR-012, FR-013, FR-038,
// FR-039, contracts/cli.md): spec US1 scenarios 1, 2, 3, 6 and 7 over a fake bank.Provider, an
// httptest Firefly III, a recording notifier, a fixed clock (2026-09-22 10:00 Europe/Vilnius, a
// 30-day window and a 3-day tolerance unless a case says otherwise) and bytes.Buffer log writers.
// It is the RED half of T052: app.Deps, app.CheckOptions and app.Check do not exist yet.
//
// Decisions this suite pins for T052: the run summary is one INFO record in the file log whose
// top-level attrs are window (the string "<from>..<to>"), accounts_checked, accounts_unchecked,
// matched, missing, deduplicated and void; Deps.Now is called exactly once per run; Firefly III
// accounts are listed before any bank call; an unmapped account costs no bank call.
type CheckSuite struct {
	suite.Suite

	vilnius *time.Location
}

// SetupSuite loads the configured time zone once.
func (s *CheckSuite) SetupSuite() {
	loc, err := time.LoadLocation("Europe/Vilnius")
	s.Require().NoError(err)

	s.vilnius = loc
}

// TestScenario1MatchWithinToleranceSendsNothing encodes US1 scenario 1: a bank −12.40 EUR on
// 2026-09-20 and a Firefly III withdrawal of 12.40 EUR on 2026-09-21 match under a 3-day tolerance,
// so nothing is reminded.
func (s *CheckSuite) TestScenario1MatchWithinToleranceSendsNothing() {
	h := s.newHarness(
		[]bank.Transaction{s.bankTx("2026-09-20", "-12.40", bank.Booked, "ref-1", "ANON BAKERY")},
		[]ffEntry{{group: "501", kind: "withdrawal", date: "2026-09-21", amount: "12.40", description: "FF BAKERY"}},
	)

	rep, code := h.run(s.T().Context(), app.CheckOptions{})

	s.Equal(0, code)
	s.Empty(h.notifier.digests, "a matched transaction must not be reminded")
	s.Equal(report.Summary{AccountsChecked: 1, Matched: 1}, rep.Summary())
	s.Require().Len(rep.Accounts, 1)
	s.Require().Len(rep.Accounts[0].Result.Matched, 1)
	s.Equal("501", rep.Accounts[0].Result.Matched[0].Firefly.GroupID)
	s.Empty(h.stdout.String())
	s.Empty(h.stderr.String())
}

// TestScenario2MissingIsListedWithDateAmountCurrencyAndDescription encodes US1 scenario 2: a bank
// transaction whose only same-amount Firefly III entries are the opposite direction, or beyond the
// tolerance, is listed in the digest with date, amount, currency and description, and the run
// ends "missing found".
func (s *CheckSuite) TestScenario2MissingIsListedWithDateAmountCurrencyAndDescription() {
	h := s.newHarness(
		[]bank.Transaction{s.bankTx("2026-09-19", "-63.12", bank.Booked, "ref-1", "ANON GROCERY")},
		[]ffEntry{
			{group: "601", kind: "deposit", date: "2026-09-19", amount: "63.12", description: "FF REFUND"},
			{group: "602", kind: "withdrawal", date: "2026-09-15", amount: "63.12", description: "FF GROCERY"},
		},
	)

	rep, code := h.run(s.T().Context(), app.CheckOptions{})

	s.Equal(1, code)
	s.Equal(report.Summary{AccountsChecked: 1, Missing: 1}, rep.Summary())
	s.Require().Len(h.notifier.digests, 1)

	text := h.notifier.digests[0].Text()
	s.Contains(text, checkHeading)
	s.Contains(text, "- 2026-09-19  -63.12 EUR  ANON GROCERY")
}

// TestScenario3OneOfTwoIdenticalBankTransactionsIsMissing encodes US1 scenario 3: two −5.00 EUR bank
// transactions on the same day against one matching Firefly III entry report exactly one missing.
func (s *CheckSuite) TestScenario3OneOfTwoIdenticalBankTransactionsIsMissing() {
	h := s.newHarness(
		[]bank.Transaction{
			s.bankTx("2026-09-18", "-5.00", bank.Booked, "ref-a", "ANON CAFE"),
			s.bankTx("2026-09-18", "-5.00", bank.Booked, "ref-b", "ANON CAFE"),
		},
		[]ffEntry{{group: "701", kind: "withdrawal", date: "2026-09-18", amount: "5.00", description: "FF CAFE"}},
	)

	rep, code := h.run(s.T().Context(), app.CheckOptions{})

	s.Equal(1, code)
	s.Equal(report.Summary{AccountsChecked: 1, Matched: 1, Missing: 1}, rep.Summary())
	s.Require().Len(h.notifier.digests, 1)

	lines := 0

	for _, line := range h.notifier.digests[0].Lines {
		if strings.HasPrefix(line, "- 2026-09-18  -5.00 EUR  ANON CAFE") {
			lines++
		}
	}

	s.Equal(1, lines, "exactly one of the two identical transactions is missing")
}

// TestScenario6CleanRunIsSilent encodes US1 scenario 6 and the clean-run case: every bank
// transaction has a counterpart and the consent is far from expiry, so no notifier is called, the
// run exits 0 and nothing reaches stdout or stderr (FR-039).
func (s *CheckSuite) TestScenario6CleanRunIsSilent() {
	h := s.newHarness(
		[]bank.Transaction{
			s.bankTx("2026-09-20", "-12.40", bank.Booked, "ref-1", "ANON BAKERY"),
			s.bankTx("2026-09-10", "100.00", bank.Booked, "ref-2", "ANON SALARY"),
			s.bankTx("2026-08-24", "-7.99", bank.Booked, "ref-3", "ANON STREAMING"),
		},
		[]ffEntry{
			{group: "801", kind: "withdrawal", date: "2026-09-20", amount: "12.40", description: "FF BAKERY"},
			{group: "802", kind: "deposit", date: "2026-09-11", amount: "100.00", description: "FF SALARY"},
			{group: "803", kind: "withdrawal", date: "2026-08-22", amount: "7.99", description: "FF STREAMING"},
		},
	)

	rep, code := h.run(s.T().Context(), app.CheckOptions{})

	s.Equal(0, code)
	s.False(rep.DigestNeeded())
	s.Equal(report.Summary{AccountsChecked: 1, Matched: 3}, rep.Summary())
	s.Empty(h.notifier.digests, "a clean run calls no notifier")
	s.Empty(h.stdout.String(), "a clean run writes nothing to stdout")
	s.Empty(h.stderr.String(), "a clean run writes nothing to stderr")
}

// TestScenario7MissingIsReportedAgainOnTheNextRun encodes US1 scenario 7 and FR-012: nothing is
// remembered between runs, so a transaction still not entered is reported again, identically, and
// the state Check was given is left untouched.
func (s *CheckSuite) TestScenario7MissingIsReportedAgainOnTheNextRun() {
	h := s.newHarness(
		[]bank.Transaction{s.bankTx("2026-09-19", "-63.12", bank.Booked, "ref-1", "ANON GROCERY")},
		nil,
	)

	before := s.cloneState(h.st)

	_, first := h.run(s.T().Context(), app.CheckOptions{})
	_, second := h.run(s.T().Context(), app.CheckOptions{})

	s.Equal(1, first)
	s.Equal(1, second)
	s.Require().Len(h.notifier.digests, 2, "each run delivers its own digest")
	s.Contains(h.notifier.digests[0].Text(), "- 2026-09-19  -63.12 EUR  ANON GROCERY")
	s.Equal(h.notifier.digests[0], h.notifier.digests[1], "the second run reports the same transaction again")
	s.Equal(before, h.st, "check never modifies the state")
}

// TestMissingFoundDeliversOneDigestQuietly covers the missing-found row of contracts/cli.md: exit
// 1, exactly one digest delivered, identical to rendering the returned report with Config.Banks,
// and nothing on stdout or stderr.
func (s *CheckSuite) TestMissingFoundDeliversOneDigestQuietly() {
	h := s.newHarness(
		[]bank.Transaction{
			s.bankTx("2026-09-20", "-12.40", bank.Booked, "ref-1", "ANON BAKERY"),
			s.bankTx("2026-09-19", "-63.12", bank.Booked, "ref-2", "ANON GROCERY"),
		},
		[]ffEntry{{group: "501", kind: "withdrawal", date: "2026-09-20", amount: "12.40", description: "FF BAKERY"}},
	)

	rep, code := h.run(s.T().Context(), app.CheckOptions{})

	s.Equal(1, code)
	s.Require().Len(h.notifier.digests, 1, "exactly one digest is delivered")
	s.Equal(digest.Render(rep, h.cfg.Banks), h.notifier.digests[0])
	s.Contains(h.notifier.digests[0].Text(), "- 2026-09-19  -63.12 EUR  ANON GROCERY")
	s.Equal(1, rep.Delivery.Attempted)
	s.Equal(1, rep.Delivery.Succeeded)
	s.Empty(h.stdout.String(), "a delivered missing-found run writes nothing to stdout")
	s.Empty(h.stderr.String(), "a delivered missing-found run writes nothing to stderr")
}

// TestStdoutPrintsTheDigestInsteadOfSending covers FR-013: with opts.Stdout the digest, if any, goes
// to Deps.Stdout, no notifier is called, and the exit code is the one a sending run would give.
func (s *CheckSuite) TestStdoutPrintsTheDigestInsteadOfSending() {
	cases := []struct {
		name    string
		bankTxs []bank.Transaction
		code    int
		printed bool
	}{
		{
			name:    "missing found",
			bankTxs: []bank.Transaction{s.bankTx("2026-09-19", "-63.12", bank.Booked, "ref-1", "ANON GROCERY")},
			code:    1,
			printed: true,
		},
		{
			name:    "clean",
			bankTxs: []bank.Transaction{s.bankTx("2026-09-20", "-12.40", bank.Booked, "ref-1", "ANON BAKERY")},
			code:    0,
			printed: false,
		},
	}

	for _, tc := range cases {
		s.Run(tc.name, func() {
			h := s.newHarness(tc.bankTxs, []ffEntry{
				{group: "501", kind: "withdrawal", date: "2026-09-20", amount: "12.40", description: "FF BAKERY"},
			})

			rep, code := h.run(s.T().Context(), app.CheckOptions{Stdout: true})

			s.Equal(tc.code, code)
			s.Empty(h.notifier.digests, "--stdout never calls a notifier")
			s.Empty(h.stderr.String())

			if tc.printed {
				s.Equal(digest.Render(rep, h.cfg.Banks).Text(), h.stdout.String())
				s.Contains(h.stdout.String(), "- 2026-09-19  -63.12 EUR  ANON GROCERY")
			} else {
				s.Empty(h.stdout.String(), "no digest means nothing printed")
			}
		})
	}
}

// TestWindowIsComputedOnceFromTheInjectedClock covers FR-004: Deps.Now is read exactly once, and
// the window is the last window_days days up to and including today in the configured zone, even
// when the clock's own location is a different day.
func (s *CheckSuite) TestWindowIsComputedOnceFromTheInjectedClock() {
	cases := []struct {
		name       string
		clock      time.Time
		windowDays int
		want       domain.Window
	}{
		{
			name:       "fixed clock",
			clock:      time.Date(2026, 9, 22, 10, 0, 0, 0, s.vilnius),
			windowDays: 30,
			want:       domain.Window{From: s.date("2026-08-24"), To: s.date("2026-09-22")},
		},
		{
			name:       "utc instant already tomorrow in vilnius",
			clock:      time.Date(2026, 9, 22, 21, 30, 0, 0, time.UTC),
			windowDays: 30,
			want:       domain.Window{From: s.date("2026-08-25"), To: s.date("2026-09-23")},
		},
		{
			name:       "configured window length",
			clock:      time.Date(2026, 9, 22, 10, 0, 0, 0, s.vilnius),
			windowDays: 7,
			want:       domain.Window{From: s.date("2026-09-16"), To: s.date("2026-09-22")},
		},
	}

	for _, tc := range cases {
		s.Run(tc.name, func() {
			h := s.newHarness(nil, nil)
			h.clock = tc.clock
			h.cfg.WindowDays = tc.windowDays

			rep, code := h.run(s.T().Context(), app.CheckOptions{})

			s.Equal(0, code)
			s.Equal(tc.want, rep.Window)
			s.Equal(1, h.nowCalls, "the clock is read once per run")
		})
	}
}

// TestFireflyIsQueriedFromWindowStartMinusToleranceToTodayPlusTolerance covers FR-006: the mapped
// account's Firefly III transactions are requested for [window.From − tolerance, today + tolerance].
func (s *CheckSuite) TestFireflyIsQueriedFromWindowStartMinusToleranceToTodayPlusTolerance() {
	cases := []struct {
		name      string
		tolerance int
		want      txQuery
	}{
		{name: "tolerance 3", tolerance: 3, want: txQuery{checkFireflyID, "2026-08-21", "2026-09-25"}},
		{name: "tolerance 5", tolerance: 5, want: txQuery{checkFireflyID, "2026-08-19", "2026-09-27"}},
	}

	for _, tc := range cases {
		s.Run(tc.name, func() {
			h := s.newHarness(nil, nil)
			h.cfg.DateToleranceDays = tc.tolerance

			_, code := h.run(s.T().Context(), app.CheckOptions{})

			s.Equal(0, code)
			s.Equal([]txQuery{tc.want}, h.firefly.recorded())
		})
	}
}

// TestBankIsQueriedFromWindowStart covers FR-005: the provider is asked once for the mapped account,
// with the session's id, the account carrying its bank key and identifiers, and from = window.From.
func (s *CheckSuite) TestBankIsQueriedFromWindowStart() {
	h := s.newHarness(nil, nil)

	_, code := h.run(s.T().Context(), app.CheckOptions{})

	s.Equal(0, code)

	calls := h.provider.recorded()
	s.Require().Len(calls, 1)
	s.Equal(checkSessionID, calls[0].sessionID)
	s.Equal(s.date("2026-08-24"), calls[0].from)
	s.Equal(checkBankKey, calls[0].account.BankKey)
	s.Equal(checkAccountUID, calls[0].account.UID)
	s.Equal(checkAccountHash, calls[0].account.Hash)
	s.Equal(checkAccountIBAN, calls[0].account.IBAN)
	s.Equal("EUR", calls[0].account.Currency)
}

// TestFireflyAccountsAreListedBeforeAnyBankCall pins T052's pipeline order: Firefly III accounts
// come first, so an unreachable Firefly III never costs bank quota, and such a run never exits 0
// or 1.
func (s *CheckSuite) TestFireflyAccountsAreListedBeforeAnyBankCall() {
	s.Run("reachable", func() {
		h := s.newHarness(nil, nil)

		_, code := h.run(s.T().Context(), app.CheckOptions{})

		s.Equal(0, code)

		events := h.events.all()
		s.Require().NotEmpty(events)
		s.Equal("firefly accounts", events[0], "Firefly III accounts are listed before anything else")
		s.ElementsMatch([]string{
			"firefly accounts",
			"bank " + checkAccountUID,
			"firefly transactions " + checkFireflyID,
		}, events)
	})

	s.Run("unreachable", func() {
		h := s.newHarness(nil, nil)
		h.firefly.accountsStatus = http.StatusUnauthorized

		_, code := h.run(s.T().Context(), app.CheckOptions{})

		s.Equal(2, code)
		s.Empty(h.provider.recorded(), "no bank call once Firefly III accounts cannot be listed")
	})
}

// TestSummaryIsLoggedOnceAtInfoWithExactCounts covers FR-038 and FR-039: the file log holds exactly
// one INFO summary record, with the window and every count, over a run that exercises each outcome:
// matched, missing, a pending copy deduplicated against its booked twin, a void transaction, and an
// unmapped account that costs no bank call.
func (s *CheckSuite) TestSummaryIsLoggedOnceAtInfoWithExactCounts() {
	h := s.mixedHarness()

	rep, _ := h.run(s.T().Context(), app.CheckOptions{})

	s.Equal(report.Summary{
		AccountsChecked:   1,
		AccountsUnchecked: 1,
		Matched:           2,
		Missing:           1,
		Deduplicated:      1,
		Void:              1,
	}, rep.Summary())
	s.Len(h.provider.recorded(), 1, "an unmapped account costs no bank call")

	var summaries []map[string]any

	for _, rec := range s.logRecords(h.fileLog) {
		if _, ok := rec[summaryKey]; ok && rec["level"] == slog.LevelInfo.String() {
			summaries = append(summaries, rec)
		}
	}

	s.Require().Len(summaries, 1, "exactly one INFO summary record in the file log")

	got := map[string]any{}
	for _, key := range []string{
		"window", "accounts_checked", "accounts_unchecked", "matched", "missing", "deduplicated", "void",
	} {
		got[key] = summaries[0][key]
	}

	s.Equal(map[string]any{
		"window":             "2026-08-24..2026-09-22",
		"accounts_checked":   float64(1),
		"accounts_unchecked": float64(1),
		"matched":            float64(2),
		"missing":            float64(1),
		"deduplicated":       float64(1),
		"void":               float64(1),
	}, got)
}

// TestLogsNeverContainDescriptionsOrFullIBANs covers constitution §V: no bank or Firefly III
// description, no unmasked IBAN, and no full session id or account hash reaches the file log (at
// DEBUG) or stderr, on a sending run or a --stdout run.
func (s *CheckSuite) TestLogsNeverContainDescriptionsOrFullIBANs() {
	for _, opts := range []app.CheckOptions{{}, {Stdout: true}} {
		name := "send"
		if opts.Stdout {
			name = "stdout"
		}

		s.Run(name, func() {
			h := s.mixedHarness()

			_, _ = h.run(s.T().Context(), opts)

			s.NotEmpty(h.fileLog.String(), "the run must log at least its summary")

			for _, secret := range []string{
				"ANON BAKERY", "ANON GROCERY", "ANON TAXI", "ANON CANCELLED", "ANON SPARE",
				"FF BAKERY", "FF TAXI",
				checkAccountIBAN, unmappedAccountIBAN, checkFireflyToken,
				checkSessionID, checkAccountHash,
			} {
				s.NotContains(h.fileLog.String(), secret)
				s.NotContains(h.stderr.String(), secret)
			}
		})
	}
}

// newHarness wires one case: the configured bank with one mapped account scripted with bankTxs,
// Firefly III account #1 carrying entries, a recording notifier and the fixed clock.
func (s *CheckSuite) newHarness(bankTxs []bank.Transaction, entries []ffEntry) *checkHarness {
	s.T().Helper()

	accounts, err := os.ReadFile(filepath.Clean(checkAccountsFile))
	s.Require().NoError(err)

	events := &eventLog{}
	ff := &fakeFirefly{events: events, accountsBody: accounts, entries: entries}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/accounts", ff.serveAccounts)
	mux.HandleFunc("GET /api/v1/accounts/{id}/transactions", ff.serveTransactions)

	ff.srv = httptest.NewServer(mux)
	s.T().Cleanup(ff.srv.Close)

	return &checkHarness{
		cfg: &config.Config{
			Timezone:          "Europe/Vilnius",
			WindowDays:        30,
			DateToleranceDays: 3,
			ConsentWarnDays:   7,
			LogLevel:          "debug",
			Firefly:           config.Firefly{URL: ff.srv.URL + "/api/v1"},
			Banks: map[string]config.Bank{
				checkBankKey: {Name: "Test Bank", Country: "LT", Display: "Testbank"},
			},
			Location: s.vilnius,
		},
		st: &state.State{
			Version: state.CurrentVersion,
			Sessions: map[string]state.Session{
				checkBankKey: {
					Provider:     "enablebanking",
					SessionID:    checkSessionID,
					ValidUntil:   time.Date(2027, 3, 20, 0, 0, 0, 0, time.UTC),
					AuthorizedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
					Accounts: []state.Account{{
						UID:      checkAccountUID,
						Hash:     checkAccountHash,
						IBAN:     checkAccountIBAN,
						Currency: "EUR",
						Name:     "Main",
					}},
				},
			},
		},
		events:   events,
		provider: &fakeProvider{events: events, txs: map[string][]bank.Transaction{checkAccountUID: bankTxs}},
		firefly:  ff,
		notifier: &recordingNotifier{},
		fileLog:  &bytes.Buffer{},
		stderr:   &bytes.Buffer{},
		stdout:   &bytes.Buffer{},
		clock:    time.Date(2026, 9, 22, 10, 0, 0, 0, s.vilnius),
	}
}

// mixedHarness is a run exercising every outcome: on the mapped account, two matched transactions
// (one of them the booked twin of a deduplicated pending copy), one missing and one void; plus an
// unmapped account.
func (s *CheckSuite) mixedHarness() *checkHarness {
	s.T().Helper()

	h := s.newHarness(
		[]bank.Transaction{
			s.bankTx("2026-09-20", "-12.40", bank.Booked, "ref-1", "ANON BAKERY"),
			s.bankTx("2026-09-19", "-63.12", bank.Booked, "ref-2", "ANON GROCERY"),
			s.bankTx("2026-09-17", "-8.00", bank.Pending, "ref-3", "ANON TAXI"),
			s.bankTx("2026-09-18", "-8.00", bank.Booked, "ref-3", "ANON TAXI"),
			s.bankTx("2026-09-16", "-20.00", bank.Void, "ref-4", "ANON CANCELLED"),
		},
		[]ffEntry{
			{group: "501", kind: "withdrawal", date: "2026-09-20", amount: "12.40", description: "FF BAKERY"},
			{group: "502", kind: "withdrawal", date: "2026-09-17", amount: "8.00", description: "FF TAXI"},
		},
	)
	h.addUnmappedAccount([]bank.Transaction{s.bankTx("2026-09-15", "-3.00", bank.Booked, "ref-9", "ANON SPARE")})

	return h
}

// bankTx builds one EUR bank transaction; the fake provider stamps its Account.
func (s *CheckSuite) bankTx(date, amount string, status bank.Status, ref, description string) bank.Transaction {
	s.T().Helper()

	value, err := domain.ParseAmount(amount, "EUR")
	s.Require().NoError(err)

	return bank.Transaction{
		Date:        s.date(date),
		Amount:      value,
		Status:      status,
		EntryRef:    ref,
		Description: description,
	}
}

// date parses a YYYY-MM-DD civil date.
func (s *CheckSuite) date(raw string) civil.Date {
	s.T().Helper()

	d, err := civil.ParseDate(raw)
	s.Require().NoError(err)

	return d
}

// cloneState deep-copies st through JSON, so a later comparison sees any mutation Check makes.
func (s *CheckSuite) cloneState(st *state.State) *state.State {
	s.T().Helper()

	raw, err := json.Marshal(st)
	s.Require().NoError(err)

	var out state.State
	s.Require().NoError(json.Unmarshal(raw, &out))

	return &out
}

// logRecords decodes every JSON line of the file log.
func (s *CheckSuite) logRecords(buf *bytes.Buffer) []map[string]any {
	s.T().Helper()

	var records []map[string]any

	for line := range strings.Lines(buf.String()) {
		var rec map[string]any
		s.Require().NoError(json.Unmarshal([]byte(line), &rec), "every file log line is one JSON record")

		records = append(records, rec)
	}

	return records
}
