package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/stretchr/testify/suite"

	"github.com/Toshik1978/firefly-jar/internal/app"
	"github.com/Toshik1978/firefly-jar/internal/bank"
	"github.com/Toshik1978/firefly-jar/internal/civil"
	"github.com/Toshik1978/firefly-jar/internal/config"
	"github.com/Toshik1978/firefly-jar/internal/domain"
	"github.com/Toshik1978/firefly-jar/internal/firefly"
	"github.com/Toshik1978/firefly-jar/internal/logging"
	"github.com/Toshik1978/firefly-jar/internal/notify"
	"github.com/Toshik1978/firefly-jar/internal/redact"
	"github.com/Toshik1978/firefly-jar/internal/report"
	"github.com/Toshik1978/firefly-jar/internal/state"
)

// Anonymized identifiers of the two-bank isolation fixture: bank A carries two accounts (so a
// per-account bank failure can be scripted without tripping the consent-loss skip that
// ConsentSuite already covers for ErrConsentExpired/ErrConsentRevoked), bank B carries one account
// that is always reconciled normally, so a case can prove bank A's failure never reaches it.
const (
	isoBankA = "banka"
	isoBankB = "bankb"

	isoSessionA = "00000000-0000-0000-0000-0000000002a0"
	isoSessionB = "00000000-0000-0000-0000-0000000002b0"

	isoUIDA1 = "00000000-0000-0000-0000-000000000210"
	isoUIDA2 = "00000000-0000-0000-0000-000000000211"
	isoUIDB1 = "00000000-0000-0000-0000-000000000220"

	isoIBANA1 = "LT000000000000000210"
	isoIBANA2 = "LT000000000000000211"
	isoIBANB1 = "LT000000000000000220"

	isoFFIDA1 = "1"
	isoFFIDA2 = "2"
	isoFFIDB1 = "3"

	// isoHugeTotalPages is a meta.pagination.total_pages value the fake Firefly III always reports
	// for a data-incomplete-scripted account, comfortably past firefly.maxPages (200), so
	// fetchPages hits the page cap with more still to come and reports ErrDataIncomplete without
	// any status code, retry or real sleep involved.
	isoHugeTotalPages = 999
)

// isoFireflyAccount is one Firefly III asset account the isolation fake accounts endpoint serves.
type isoFireflyAccount struct {
	id       string
	iban     string
	currency string
	name     string
}

// isolationFirefly is an httptest Firefly III whose transactions endpoint can be scripted to fail
// for one specific account id, either with an HTTP status (txStatus) or by reporting a
// meta.pagination.total_pages past firefly's page cap (dataIncomplete, yielding ErrDataIncomplete),
// while serving every other account's entries normally, so a case can prove a Firefly III failure
// for one account never affects another.
type isolationFirefly struct {
	srv            *httptest.Server
	events         *eventLog
	accountsBody   []byte
	accountsStatus int
	txStatus       map[string]int
	dataIncomplete map[string]bool
	entries        map[string][]ffEntry

	mu      sync.Mutex
	queries []txQuery
}

// recorded returns a copy of every transactions query so far.
func (f *isolationFirefly) recorded() []txQuery {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]txQuery(nil), f.queries...)
}

// serveAccounts serves GET /accounts: the fixture, or accountsStatus when a case sets one.
func (f *isolationFirefly) serveAccounts(w http.ResponseWriter, _ *http.Request) {
	f.events.add("firefly accounts")

	w.Header().Set("Content-Type", "application/json")

	if f.accountsStatus != 0 {
		w.WriteHeader(f.accountsStatus)
		_, _ = w.Write([]byte(`{"message":"Unauthenticated."}`))

		return
	}

	_, _ = w.Write(f.accountsBody)
}

// serveTransactions serves GET /accounts/{id}/transactions: txStatus[id] when a case scripts an
// HTTP failure for this account id, an always-more-pages body when the case scripts
// dataIncomplete[id], otherwise one page built from entries[id].
func (f *isolationFirefly) serveTransactions(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	f.events.add("firefly transactions " + id)

	f.mu.Lock()
	f.queries = append(f.queries, txQuery{
		accountID: id,
		start:     r.URL.Query().Get("start"),
		end:       r.URL.Query().Get("end"),
	})
	f.mu.Unlock()

	if status, ok := f.txStatus[id]; ok {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"message":"error"}`))

		return
	}

	var page map[string]any
	if f.dataIncomplete[id] {
		// Every page claims isoHugeTotalPages remain, so fetchPages keeps requesting pages until it
		// hits firefly's own maxPages cap and reports ErrDataIncomplete -- no status code, retry or
		// real sleep involved.
		page = map[string]any{
			"data": []any{},
			"meta": map[string]any{"pagination": map[string]any{"total_pages": isoHugeTotalPages}},
		}
	} else {
		page = groupsPage(id, f.entries[id])
	}

	body, err := json.Marshal(page)
	if err != nil {
		http.Error(w, "encode", http.StatusInternalServerError)

		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

// isolationSetup is one case's fixture: the configured banks and their saved sessions, the Firefly
// III accounts the fake serves, an optional Firefly III accounts-endpoint failure status, a
// per-Firefly-account-id transactions failure status, the Firefly-account-ids scripted to report
// ErrDataIncomplete instead, per-Firefly-account-id entries, and a per-bank-account-uid bank
// provider script.
type isolationSetup struct {
	banks             map[string]config.Bank
	sessions          map[string]state.Session
	ffAccounts        []isoFireflyAccount
	accountsStatus    int
	txStatus          map[string]int
	dataIncompleteIDs []string
	entries           map[string][]ffEntry
	scripts           map[string]consentAccountScript
}

// isolationHarness is one case's wiring: config and state built from an isolationSetup, a
// consentProvider so a case can script a bank failure per account, an isolationFirefly so a case
// can script a Firefly III failure per account, a recording notifier and the three output
// buffers, all read against the fixed clock (2026-09-22 10:00 Europe/Vilnius).
type isolationHarness struct {
	cfg      *config.Config
	st       *state.State
	events   *eventLog
	provider *consentProvider
	firefly  *isolationFirefly
	notifier *recordingNotifier
	fileLog  *bytes.Buffer
	stderr   *bytes.Buffer
	stdout   *bytes.Buffer
	clock    time.Time
}

// run executes one check with fresh Deps over the harness.
func (h *isolationHarness) run(ctx context.Context) (report.RunReport, int) {
	return app.Check(ctx, h.deps(), app.CheckOptions{})
}

// deps wires the harness into app.Deps, the same way consent_test.go's consentHarness does: the
// same production logging.New, so a stderr assertion here sees exactly what a real run would
// write.
func (h *isolationHarness) deps() app.Deps {
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
			return h.clock
		},
		Stdout: h.stdout,
	}
}

// IsolationSuite covers app.Check's failure isolation and the stderr ERROR summary (T070, FR-032,
// FR-033, contracts/cli.md): a bank-level provider failure (ErrRateLimited, a generic *bank.Error,
// ErrDataIncomplete) unchecks only that bank's accounts while an unrelated bank is still
// reconciled and reported; a Firefly III accounts-listing failure is a problem-only run that costs
// zero bank calls; a Firefly III transactions failure for one account unchecks only that account;
// and every exit-2 run prints exactly one ERROR summary line to stderr, while an exit 0 or 1 run
// stays silent there. It is the RED half of T071: today check.go and check_account.go already
// isolate most of these failures (fix rounds on fj-xwu.5 and fj-xwu.4), but nothing yet logs the
// ERROR summary line, so every stderr assertion below is red until T071 adds it.
type IsolationSuite struct {
	suite.Suite

	vilnius *time.Location
}

// SetupSuite loads the configured time zone once.
func (s *IsolationSuite) SetupSuite() {
	loc, err := time.LoadLocation("Europe/Vilnius")
	s.Require().NoError(err)

	s.vilnius = loc
}

// TestBankFailureUnchecksOnlyThatBankWhileTheOtherBankIsReconciled covers the first three design
// cases at once: bank A's provider fails identically on both of its accounts (ErrRateLimited, a
// generic *bank.Error whose Detail carries an IBAN, or ErrDataIncomplete), both are unchecked with
// the matching code and detail, bank B's account is still reconciled and its missing transaction
// still reported, and the run exits 2.
func (s *IsolationSuite) TestBankFailureUnchecksOnlyThatBankWhileTheOtherBankIsReconciled() {
	genericDetail := "gateway timeout for account " + isoIBANA1

	cases := []struct {
		name       string
		err        error
		wantCode   report.UncheckedCode
		wantDetail string
	}{
		{name: "rate limited", err: bank.ErrRateLimited, wantCode: report.RateLimited, wantDetail: ""},
		{
			name:       "generic bank error redacts its detail",
			err:        &bank.Error{Kind: errors.New("gateway failure"), Detail: genericDetail},
			wantCode:   report.BankError,
			wantDetail: "gateway timeout for account " + redact.MaskIBAN(isoIBANA1),
		},
		{
			name:       "bank data incomplete",
			err:        bank.ErrDataIncomplete,
			wantCode:   report.BankDataIncomplete,
			wantDetail: "",
		},
	}

	for _, tc := range cases {
		s.Run(tc.name, func() {
			h := s.newHarness(isolationSetup{
				banks: map[string]config.Bank{
					isoBankA: {Name: "Bank A", Country: "LT", Display: "Bank A"},
					isoBankB: {Name: "Bank B", Country: "LT", Display: "Bank B"},
				},
				sessions: map[string]state.Session{
					isoBankA: s.session(isoSessionA, s.account(isoUIDA1, "hash-iso-a1", isoIBANA1, "A1"),
						s.account(isoUIDA2, "hash-iso-a2", isoIBANA2, "A2")),
					isoBankB: s.session(isoSessionB, s.account(isoUIDB1, "hash-iso-b1", isoIBANB1, "B1")),
				},
				ffAccounts: []isoFireflyAccount{
					{id: isoFFIDA1, iban: isoIBANA1, currency: "EUR", name: "A1"},
					{id: isoFFIDA2, iban: isoIBANA2, currency: "EUR", name: "A2"},
					{id: isoFFIDB1, iban: isoIBANB1, currency: "EUR", name: "B1"},
				},
				scripts: map[string]consentAccountScript{
					isoUIDA1: {err: tc.err},
					isoUIDA2: {err: tc.err},
					isoUIDB1: {txs: []bank.Transaction{s.bankTx("2026-09-19", "-42.00", "ref-b1", "ANON B1 SPEND")}},
				},
			})

			rep, code := h.run(s.T().Context())

			s.Equal(2, code)

			byUID := s.accountsByUID(rep)

			s.Require().Contains(byUID, isoUIDA1)
			s.Require().NotNil(byUID[isoUIDA1].Unchecked)
			s.Equal(tc.wantCode, byUID[isoUIDA1].Unchecked.Code)
			s.Equal(tc.wantDetail, byUID[isoUIDA1].Unchecked.Detail)

			s.Require().Contains(byUID, isoUIDA2)
			s.Require().NotNil(byUID[isoUIDA2].Unchecked)
			s.Equal(tc.wantCode, byUID[isoUIDA2].Unchecked.Code)

			s.Require().Contains(byUID, isoUIDB1)
			s.Nil(byUID[isoUIDB1].Unchecked, "bank B must be fully reconciled despite bank A's failure")
			s.Require().Len(byUID[isoUIDB1].Result.Missing, 1)

			s.False(h.firefly.recordedAny(isoFFIDA1), "a failing bank account must cost no Firefly III call")
			s.False(h.firefly.recordedAny(isoFFIDA2), "a failing bank account must cost no Firefly III call")
			s.True(h.firefly.recordedAny(isoFFIDB1), "the unaffected bank must still be queried")

			text := h.notifier.digests[0].Text()
			s.Contains(text, tc.wantCode.Text(), "the digest must show the unchecked reason")
			s.Contains(text, "- 2026-09-19  -42.00 EUR  ANON B1 SPEND", "bank B's missing transaction must be reported")

			if tc.wantDetail != "" {
				s.Contains(text, tc.wantCode.Text()+": "+tc.wantDetail)
				s.NotContains(text, isoIBANA1, "the raw IBAN must never reach the digest")
			}
		})
	}
}

// TestFireflyAccountsUnreachableCostsZeroBankCallsAndReportsOnlyTheProblem covers the fourth
// design case: a Firefly III accounts-listing failure (a non-retried 404, so the case takes no
// real sleep) is a run-level Problem naming "Firefly III unreachable: …", no bank is ever called
// even though two are configured, no AccountResult exists at all, and the run exits 2.
func (s *IsolationSuite) TestFireflyAccountsUnreachableCostsZeroBankCallsAndReportsOnlyTheProblem() {
	h := s.newHarness(isolationSetup{
		banks: map[string]config.Bank{
			isoBankA: {Name: "Bank A", Country: "LT", Display: "Bank A"},
			isoBankB: {Name: "Bank B", Country: "LT", Display: "Bank B"},
		},
		sessions: map[string]state.Session{
			isoBankA: s.session(isoSessionA, s.account(isoUIDA1, "hash-iso-a1", isoIBANA1, "A1")),
			isoBankB: s.session(isoSessionB, s.account(isoUIDB1, "hash-iso-b1", isoIBANB1, "B1")),
		},
		accountsStatus: http.StatusNotFound,
	})

	rep, code := h.run(s.T().Context())

	s.Equal(2, code)
	s.Empty(h.provider.recorded(), "an unreachable Firefly III must cost zero bank calls")
	s.Empty(rep.Accounts, "an unreachable Firefly III leaves no account result at all")
	s.Require().Len(rep.Problems, 1)
	s.Equal("firefly", rep.Problems[0].Scope)
	s.Contains(rep.Problems[0].Reason, "Firefly III unreachable: ")

	s.Require().Len(h.notifier.digests, 1)
	text := h.notifier.digests[0].Text()
	s.Contains(text, "Firefly III unreachable: ")
	s.NotContains(text, "Missing in Firefly III", "a problem-only run has no missing section")
}

// TestFireflyTokenRejectedIsReportedAsUnauthorized covers a 401 or 403 from the accounts listing:
// the server answered, so "unreachable" would send the owner looking at the network. The problem
// says the token was rejected and where it comes from, and the run still costs no bank call.
func (s *IsolationSuite) TestFireflyTokenRejectedIsReportedAsUnauthorized() {
	const want = "Firefly III unauthorized: the API token was rejected — check firefly.token_file or " +
		"FIREFLY_JAR_FIREFLY_TOKEN"

	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		s.Run(http.StatusText(status), func() {
			h := s.newHarness(isolationSetup{
				banks: map[string]config.Bank{isoBankA: {Name: "Bank A", Country: "LT", Display: "Bank A"}},
				sessions: map[string]state.Session{
					isoBankA: s.session(isoSessionA, s.account(isoUIDA1, "hash-iso-a1", isoIBANA1, "A1")),
				},
				accountsStatus: status,
			})

			rep, code := h.run(s.T().Context())

			s.Equal(2, code)
			s.Empty(h.provider.recorded(), "a rejected token must cost zero bank calls")
			s.Require().Len(rep.Problems, 1)
			s.Equal(report.Problem{Scope: "firefly", Reason: want}, rep.Problems[0])

			s.Require().Len(h.notifier.digests, 1)
			text := h.notifier.digests[0].Text()
			s.Contains(text, "- firefly: "+want)
			s.NotContains(text, "unreachable")
		})
	}
}

// TestFireflyTransactionsFailureForOneAccountUnchecksOnlyThatAccount covers the fifth design case
// for both ways it can happen: a non-retried 404 (FireflyError) and a page count past firefly's
// own page cap (FireflyDataIncomplete, fix round 1 of T070 review). Either way, only that one
// account is unchecked, the bank's other account is fully reconciled, and the bank provider is
// still called for both since the failure is Firefly III's, never the bank's.
func (s *IsolationSuite) TestFireflyTransactionsFailureForOneAccountUnchecksOnlyThatAccount() {
	cases := []struct {
		name              string
		txStatus          map[string]int
		dataIncompleteIDs []string
		wantCode          report.UncheckedCode
	}{
		{
			name:     "not found",
			txStatus: map[string]int{isoFFIDA1: http.StatusNotFound},
			wantCode: report.FireflyError,
		},
		{
			name:              "data incomplete",
			dataIncompleteIDs: []string{isoFFIDA1},
			wantCode:          report.FireflyDataIncomplete,
		},
	}

	for _, tc := range cases {
		s.Run(tc.name, func() {
			h := s.newHarness(isolationSetup{
				banks: map[string]config.Bank{
					isoBankA: {Name: "Bank A", Country: "LT", Display: "Bank A"},
				},
				sessions: map[string]state.Session{
					isoBankA: s.session(isoSessionA, s.account(isoUIDA1, "hash-iso-a1", isoIBANA1, "A1"),
						s.account(isoUIDA2, "hash-iso-a2", isoIBANA2, "A2")),
				},
				ffAccounts: []isoFireflyAccount{
					{id: isoFFIDA1, iban: isoIBANA1, currency: "EUR", name: "A1"},
					{id: isoFFIDA2, iban: isoIBANA2, currency: "EUR", name: "A2"},
				},
				txStatus:          tc.txStatus,
				dataIncompleteIDs: tc.dataIncompleteIDs,
				entries: map[string][]ffEntry{
					isoFFIDA2: {
						{group: "901", kind: "withdrawal", date: "2026-09-20", amount: "12.40", description: "FF A2"},
					},
				},
				scripts: map[string]consentAccountScript{
					isoUIDA1: {},
					isoUIDA2: {txs: []bank.Transaction{s.bankTx("2026-09-20", "-12.40", "ref-a2", "ANON A2 SPEND")}},
				},
			})

			rep, code := h.run(s.T().Context())

			s.Equal(2, code)

			byUID := s.accountsByUID(rep)

			s.Require().Contains(byUID, isoUIDA1)
			s.Require().NotNil(byUID[isoUIDA1].Unchecked)
			s.Equal(tc.wantCode, byUID[isoUIDA1].Unchecked.Code)

			s.Require().Contains(byUID, isoUIDA2)
			s.Nil(byUID[isoUIDA2].Unchecked, "the other account must be fully reconciled")
			s.Require().Len(byUID[isoUIDA2].Result.Matched, 1)

			s.True(h.provider.hasCallFor(isoUIDA1), "the bank must still be called: this is a Firefly III failure")
			s.True(h.provider.hasCallFor(isoUIDA2))
			s.True(h.firefly.recordedAny(isoFFIDA1))
			s.True(h.firefly.recordedAny(isoFFIDA2))

			s.Require().Len(h.notifier.digests, 1)
			s.Contains(h.notifier.digests[0].Text(), "unchecked — "+tc.wantCode.Text())
		})
	}
}

// TestExactlyOneErrorSummaryLineOnExit2AndNoneOnExit0Or1 pins the ERROR summary line contracts/
// cli.md requires for every exit-2 run: the process logger's stderr text handler (WARN and above,
// FR-038/FR-039) carries exactly one ERROR record, message "check failed", with the run's
// unchecked-account count, run-level-problem count and failed-delivery count as its attrs, in that
// order (unchecked, problems, delivery_failed) — a call shaped
// `Log.ErrorContext(ctx, "check failed", "unchecked", n, "problems", m, "delivery_failed", k)`, so
// T071's implementation renders exactly
// `level=ERROR msg="check failed" unchecked=<n> problems=<m> delivery_failed=<k>` through
// logging.New's existing text handler. A clean run and a missing-found run that delivers
// successfully write nothing to stderr at all, per FR-039.
func (s *IsolationSuite) TestExactlyOneErrorSummaryLineOnExit2AndNoneOnExit0Or1() {
	cases := []struct {
		name            string
		harness         func() *isolationHarness
		wantCode        int
		wantErrorLine   bool
		wantUnchecked   int
		wantProblems    int
		wantDeliveryBad int
	}{
		{
			name:          "exit 2: bank failure unchecks two accounts",
			harness:       s.rateLimitedTwoBankHarness,
			wantCode:      2,
			wantErrorLine: true,
			wantUnchecked: 2,
			wantProblems:  0,
		},
		{
			name:          "exit 2: firefly accounts unreachable is a run-level problem",
			harness:       s.unreachableFireflyHarness,
			wantCode:      2,
			wantErrorLine: true,
			wantUnchecked: 0,
			wantProblems:  1,
		},
		{
			name:          "exit 0: clean run stays silent",
			harness:       s.cleanTwoBankHarness,
			wantCode:      0,
			wantErrorLine: false,
		},
		{
			name:          "exit 1: missing found and delivered stays silent",
			harness:       s.missingFoundTwoBankHarness,
			wantCode:      1,
			wantErrorLine: false,
		},
	}

	for _, tc := range cases {
		s.Run(tc.name, func() {
			h := tc.harness()

			_, code := h.run(s.T().Context())

			s.Equal(tc.wantCode, code)

			lines := errorSummaryLines(h.stderr.String())

			if !tc.wantErrorLine {
				s.Empty(h.stderr.String(), "an exit 0 or 1 run must write nothing to stderr")

				return
			}

			s.Require().Len(lines, 1, "exactly one ERROR summary line must reach stderr")
			s.Regexp(
				`level=ERROR msg="check failed" unchecked=`+strconv.Itoa(tc.wantUnchecked)+
					` problems=`+strconv.Itoa(tc.wantProblems)+` delivery_failed=`+strconv.Itoa(tc.wantDeliveryBad),
				lines[0],
			)
		})
	}
}

// TestRunSummaryRecordCarriesProblemsAndDeliveryFailedCounts pins the carry-over from fj-xwu.6.2's
// review of T052 (fix round 1 of the T070 review): the INFO "run summary" record already logged by
// logSummary (FR-038, constitution §VI "plus any errors") gains problems (len(rep.Problems)) and
// delivery_failed (Delivery.Attempted − Delivery.Succeeded) alongside its existing counts, so
// T071 cannot add the stderr ERROR line without also wiring these into the one record every run
// already writes. Pinned on an exit-2 run (one run-level problem, delivery still succeeds) and a
// clean run (both zero).
func (s *IsolationSuite) TestRunSummaryRecordCarriesProblemsAndDeliveryFailedCounts() {
	cases := []struct {
		name             string
		harness          func() *isolationHarness
		wantProblems     float64
		wantDeliveryFail float64
	}{
		{
			name:             "exit 2: firefly unreachable is one run-level problem",
			harness:          s.unreachableFireflyHarness,
			wantProblems:     1,
			wantDeliveryFail: 0,
		},
		{
			name:             "exit 0: clean run has neither",
			harness:          s.cleanTwoBankHarness,
			wantProblems:     0,
			wantDeliveryFail: 0,
		},
	}

	for _, tc := range cases {
		s.Run(tc.name, func() {
			h := tc.harness()

			_, _ = h.run(s.T().Context())

			var summaries []map[string]any

			for _, rec := range s.logRecords(h.fileLog) {
				if _, ok := rec[summaryKey]; ok && rec["level"] == slog.LevelInfo.String() {
					summaries = append(summaries, rec)
				}
			}

			s.Require().Len(summaries, 1, "exactly one INFO summary record in the file log")
			s.Equal(map[string]any{"problems": tc.wantProblems, "delivery_failed": tc.wantDeliveryFail}, map[string]any{
				"problems":        summaries[0]["problems"],
				"delivery_failed": summaries[0]["delivery_failed"],
			})
		})
	}
}

// rateLimitedTwoBankHarness builds the exit-2-via-unchecked-accounts fixture the ERROR-summary
// case table reuses from TestBankFailureUnchecksOnlyThatBankWhileTheOtherBankIsReconciled.
func (s *IsolationSuite) rateLimitedTwoBankHarness() *isolationHarness {
	return s.newHarness(isolationSetup{
		banks: map[string]config.Bank{
			isoBankA: {Name: "Bank A", Country: "LT", Display: "Bank A"},
			isoBankB: {Name: "Bank B", Country: "LT", Display: "Bank B"},
		},
		sessions: map[string]state.Session{
			isoBankA: s.session(isoSessionA, s.account(isoUIDA1, "hash-iso-a1", isoIBANA1, "A1"),
				s.account(isoUIDA2, "hash-iso-a2", isoIBANA2, "A2")),
			isoBankB: s.session(isoSessionB, s.account(isoUIDB1, "hash-iso-b1", isoIBANB1, "B1")),
		},
		ffAccounts: []isoFireflyAccount{
			{id: isoFFIDA1, iban: isoIBANA1, currency: "EUR", name: "A1"},
			{id: isoFFIDA2, iban: isoIBANA2, currency: "EUR", name: "A2"},
			{id: isoFFIDB1, iban: isoIBANB1, currency: "EUR", name: "B1"},
		},
		scripts: map[string]consentAccountScript{
			isoUIDA1: {err: bank.ErrRateLimited},
			isoUIDA2: {err: bank.ErrRateLimited},
			isoUIDB1: {txs: []bank.Transaction{s.bankTx("2026-09-19", "-42.00", "ref-b1", "ANON B1 SPEND")}},
		},
	})
}

// unreachableFireflyHarness builds the exit-2-via-run-level-problem fixture the ERROR-summary case
// table reuses from TestFireflyAccountsUnreachableCostsZeroBankCallsAndReportsOnlyTheProblem.
func (s *IsolationSuite) unreachableFireflyHarness() *isolationHarness {
	return s.newHarness(isolationSetup{
		banks: map[string]config.Bank{
			isoBankA: {Name: "Bank A", Country: "LT", Display: "Bank A"},
		},
		sessions: map[string]state.Session{
			isoBankA: s.session(isoSessionA, s.account(isoUIDA1, "hash-iso-a1", isoIBANA1, "A1")),
		},
		accountsStatus: http.StatusNotFound,
	})
}

// cleanTwoBankHarness builds an exit-0 fixture: both banks' single account matches its Firefly III
// entry exactly, so nothing is missing, nothing is unchecked and no problem exists.
func (s *IsolationSuite) cleanTwoBankHarness() *isolationHarness {
	return s.newHarness(isolationSetup{
		banks: map[string]config.Bank{
			isoBankA: {Name: "Bank A", Country: "LT", Display: "Bank A"},
		},
		sessions: map[string]state.Session{
			isoBankA: s.session(isoSessionA, s.account(isoUIDA1, "hash-iso-a1", isoIBANA1, "A1")),
		},
		ffAccounts: []isoFireflyAccount{{id: isoFFIDA1, iban: isoIBANA1, currency: "EUR", name: "A1"}},
		entries: map[string][]ffEntry{
			isoFFIDA1: {{group: "801", kind: "withdrawal", date: "2026-09-20", amount: "12.40", description: "FF A1"}},
		},
		scripts: map[string]consentAccountScript{
			isoUIDA1: {txs: []bank.Transaction{s.bankTx("2026-09-20", "-12.40", "ref-a1", "ANON A1 SPEND")}},
		},
	})
}

// missingFoundTwoBankHarness builds an exit-1 fixture: the one account has a bank transaction with
// no Firefly III counterpart, delivered successfully to the recording notifier, so no problem or
// delivery failure exists despite the missing transaction.
func (s *IsolationSuite) missingFoundTwoBankHarness() *isolationHarness {
	return s.newHarness(isolationSetup{
		banks: map[string]config.Bank{
			isoBankA: {Name: "Bank A", Country: "LT", Display: "Bank A"},
		},
		sessions: map[string]state.Session{
			isoBankA: s.session(isoSessionA, s.account(isoUIDA1, "hash-iso-a1", isoIBANA1, "A1")),
		},
		ffAccounts: []isoFireflyAccount{{id: isoFFIDA1, iban: isoIBANA1, currency: "EUR", name: "A1"}},
		scripts: map[string]consentAccountScript{
			isoUIDA1: {txs: []bank.Transaction{s.bankTx("2026-09-19", "-63.12", "ref-a1", "ANON A1 GROCERY")}},
		},
	})
}

// newHarness wires one isolation case from setup, over the fixed clock (2026-09-22 10:00
// Europe/Vilnius).
func (s *IsolationSuite) newHarness(setup isolationSetup) *isolationHarness {
	s.T().Helper()

	dataIncomplete := make(map[string]bool, len(setup.dataIncompleteIDs))
	for _, id := range setup.dataIncompleteIDs {
		dataIncomplete[id] = true
	}

	events := &eventLog{}
	ff := &isolationFirefly{
		events:         events,
		accountsBody:   s.accountsBody(setup.ffAccounts...),
		accountsStatus: setup.accountsStatus,
		txStatus:       setup.txStatus,
		dataIncomplete: dataIncomplete,
		entries:        setup.entries,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/accounts", ff.serveAccounts)
	mux.HandleFunc("GET /api/v1/accounts/{id}/transactions", ff.serveTransactions)

	ff.srv = httptest.NewServer(mux)
	s.T().Cleanup(ff.srv.Close)

	return &isolationHarness{
		cfg: &config.Config{
			Timezone:          "Europe/Vilnius",
			WindowDays:        30,
			DateToleranceDays: 3,
			ConsentWarnDays:   7,
			LogLevel:          "debug",
			Firefly:           config.Firefly{URL: ff.srv.URL + "/api/v1"},
			Banks:             setup.banks,
			Location:          s.vilnius,
		},
		st:       &state.State{Version: state.CurrentVersion, Sessions: setup.sessions},
		events:   events,
		provider: &consentProvider{events: events, scripts: setup.scripts},
		firefly:  ff,
		notifier: &recordingNotifier{},
		fileLog:  &bytes.Buffer{},
		stderr:   &bytes.Buffer{},
		stdout:   &bytes.Buffer{},
		clock:    time.Date(2026, 9, 22, 10, 0, 0, 0, s.vilnius),
	}
}

// session builds one state.Session valid far into the future, so a case exercising bank or
// Firefly III failures never also trips the unrelated consent-expiry rules ConsentSuite covers.
func (s *IsolationSuite) session(sessionID string, accounts ...state.Account) state.Session {
	return state.Session{
		Provider:     "enablebanking",
		SessionID:    sessionID,
		ValidUntil:   time.Date(2027, 3, 20, 0, 0, 0, 0, time.UTC),
		AuthorizedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		Accounts:     accounts,
	}
}

// account builds one EUR state.Account identified by uid, hash and iban, named name.
func (s *IsolationSuite) account(uid, hash, iban, name string) state.Account {
	return state.Account{UID: uid, Hash: hash, IBAN: iban, Currency: "EUR", Name: name}
}

// bankTx builds one booked EUR bank.Transaction; the fake provider stamps its Account.
func (s *IsolationSuite) bankTx(date, amount, ref, description string) bank.Transaction {
	s.T().Helper()

	d, err := civil.ParseDate(date)
	s.Require().NoError(err)

	value, err := domain.ParseAmount(amount, "EUR")
	s.Require().NoError(err)

	return bank.Transaction{
		Date:        d,
		Amount:      value,
		Status:      bank.Booked,
		EntryRef:    ref,
		Description: description,
	}
}

// accountsBody renders accounts as one Firefly III GET /accounts page, the same shape
// consent_test.go's ConsentSuite.accountsBody renders.
func (s *IsolationSuite) accountsBody(accounts ...isoFireflyAccount) []byte {
	s.T().Helper()

	data := make([]map[string]any, 0, len(accounts))

	for _, a := range accounts {
		data = append(data, map[string]any{
			"type": "accounts",
			"id":   a.id,
			"attributes": map[string]any{
				"active":                  true,
				"name":                    a.name,
				"type":                    "asset",
				"account_role":            "defaultAsset",
				"currency_code":           a.currency,
				"currency_decimal_places": 2,
				"iban":                    a.iban,
			},
		})
	}

	body, err := json.Marshal(map[string]any{"data": data})
	s.Require().NoError(err)

	return body
}

// accountsByUID indexes rep's accounts by their bank account uid, for a case that needs to look up
// one account's outcome among several.
func (s *IsolationSuite) accountsByUID(rep report.RunReport) map[string]report.AccountResult {
	byUID := make(map[string]report.AccountResult, len(rep.Accounts))
	for i := range rep.Accounts {
		byUID[rep.Accounts[i].Mapping.Bank.UID] = rep.Accounts[i]
	}

	return byUID
}

// recordedAny reports whether f ever received a transactions query for accountID.
func (f *isolationFirefly) recordedAny(accountID string) bool {
	for _, q := range f.recorded() {
		if q.accountID == accountID {
			return true
		}
	}

	return false
}

// logRecords decodes every JSON line of the file log, the same way check_test.go's
// CheckSuite.logRecords does.
func (s *IsolationSuite) logRecords(buf *bytes.Buffer) []map[string]any {
	s.T().Helper()

	var records []map[string]any

	for line := range strings.Lines(buf.String()) {
		var rec map[string]any
		s.Require().NoError(json.Unmarshal([]byte(line), &rec), "every file log line is one JSON record")

		records = append(records, rec)
	}

	return records
}

// errorSummaryLines returns every non-empty line of raw containing an ERROR-level record, so a
// case can assert there is exactly one.
func errorSummaryLines(raw string) []string {
	var lines []string

	for line := range strings.SplitSeq(raw, "\n") {
		if strings.Contains(line, "level=ERROR") {
			lines = append(lines, line)
		}
	}

	return lines
}
