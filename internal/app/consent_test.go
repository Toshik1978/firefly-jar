// Package app_test's ConsentSuite covers FR-020, FR-021 and data-model.md "Consent state" (T062):
// how app.Check treats a configured bank's consent relative to a fixed clock. It is the RED half of
// T063: consentState and its integration into check.go do not exist yet, so a configured bank with
// no session, or with a session covering zero accounts, is still silently skipped rather than
// reported as a run-level Problem, and a session's ValidUntil is never compared with the clock at
// all.
package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	"github.com/stretchr/testify/suite"

	"github.com/Toshik1978/firefly-jar/internal/app"
	"github.com/Toshik1978/firefly-jar/internal/bank"
	"github.com/Toshik1978/firefly-jar/internal/civil"
	"github.com/Toshik1978/firefly-jar/internal/config"
	"github.com/Toshik1978/firefly-jar/internal/firefly"
	"github.com/Toshik1978/firefly-jar/internal/logging"
	"github.com/Toshik1978/firefly-jar/internal/notify"
	"github.com/Toshik1978/firefly-jar/internal/redact"
	"github.com/Toshik1978/firefly-jar/internal/report"
	"github.com/Toshik1978/firefly-jar/internal/state"
)

// consentAccountScript is what the fake provider does for one account UID: return txs, or err
// instead when err is non-nil (a mid-run consent failure the real Enable Banking API can report
// even while the saved session still looks valid).
type consentAccountScript struct {
	txs []bank.Transaction
	err error
}

// consentProvider is a bank.Provider that scripts a response per account UID and records every
// call it received, in order, so a case can prove the provider was never called again for a bank
// once one of its accounts failed with a consent error.
type consentProvider struct {
	mu      sync.Mutex
	events  *eventLog
	scripts map[string]consentAccountScript
	calls   []providerCall
}

// Transactions records the call and returns the account's scripted response.
func (p *consentProvider) Transactions(
	_ context.Context, sessionID string, acc bank.Account, from civil.Date,
) ([]bank.Transaction, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.calls = append(p.calls, providerCall{sessionID: sessionID, account: acc, from: from})
	p.events.add("bank " + acc.UID)

	script := p.scripts[acc.UID]
	if script.err != nil {
		return nil, script.err
	}

	out := append([]bank.Transaction(nil), script.txs...)
	for i := range out {
		out[i].Account = acc
	}

	return out, nil
}

// recorded returns a copy of every call so far.
func (p *consentProvider) recorded() []providerCall {
	p.mu.Lock()
	defer p.mu.Unlock()

	return append([]providerCall(nil), p.calls...)
}

// hasCallFor reports whether uid was ever asked for.
func (p *consentProvider) hasCallFor(uid string) bool {
	calls := p.recorded()
	for i := range calls {
		if calls[i].account.UID == uid {
			return true
		}
	}

	return false
}

// consentFireflyAccount is one Firefly III asset account the fake accounts endpoint serves, enough
// to let a bank account with the same IBAN and currency map automatically (internal/mapping).
type consentFireflyAccount struct {
	id       string
	iban     string
	currency string
	name     string
}

// consentHarness is one consent case's wiring: config and state built in code, a consentProvider so
// a case can script a mid-run consent error, an httptest Firefly III, a recording notifier and the
// three output buffers, all read against the fixed clock (2026-09-22 10:00 Europe/Vilnius).
type consentHarness struct {
	cfg      *config.Config
	st       *state.State
	events   *eventLog
	provider *consentProvider
	firefly  *fakeFirefly
	notifier *recordingNotifier
	fileLog  *bytes.Buffer
	stderr   *bytes.Buffer
	stdout   *bytes.Buffer
	clock    time.Time
}

// run executes one check with fresh Deps over the harness.
func (h *consentHarness) run(ctx context.Context) (report.RunReport, int) {
	return app.Check(ctx, h.deps(), app.CheckOptions{})
}

// deps wires the harness into app.Deps, the same way check_test.go's checkHarness does, but backed
// by a consentProvider instead of a fakeProvider.
func (h *consentHarness) deps() app.Deps {
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

// ConsentSuite covers app.Check's handling of consent state (T062, FR-020, FR-021,
// data-model.md "Consent state"), driven against a fixed clock (2026-09-22 10:00 Europe/Vilnius)
// and a consent_warn_days of 7 unless a case says otherwise: every row of the consent table, the
// mid-run provider consent error, a configured bank with no session, a configured bank whose saved
// session has zero accounts (the T063 ruling), and a state session for a bank no longer configured.
type ConsentSuite struct {
	suite.Suite

	vilnius *time.Location
}

// SetupSuite loads the configured time zone once.
func (s *ConsentSuite) SetupSuite() {
	loc, err := time.LoadLocation("Europe/Vilnius")
	s.Require().NoError(err)

	s.vilnius = loc
}

// TestConsentFarAheadOfWarnWindowIsOK covers the first row of the consent table: a ValidUntil far
// beyond consent_warn_days from now produces no warning and the account is checked normally.
func (s *ConsentSuite) TestConsentFarAheadOfWarnWindowIsOK() {
	const (
		bankKey    = "farbank"
		sessionID  = "00000000-0000-0000-0000-0000000001a0"
		accountUID = "00000000-0000-0000-0000-000000000110"
		iban       = "LT000000000000000110"
	)

	h := s.newHarness(
		map[string]config.Bank{bankKey: {Name: "Far Bank", Country: "LT", Display: "Far Bank"}},
		map[string]state.Session{
			bankKey: s.session(sessionID, time.Date(2027, 3, 20, 0, 0, 0, 0, time.UTC),
				s.account(accountUID, "hash-far", iban)),
		},
		[]consentFireflyAccount{{id: "1", iban: iban, currency: "EUR", name: "Main Account"}},
		7,
	)
	h.provider.scripts[accountUID] = consentAccountScript{}

	rep, code := h.run(s.T().Context())

	s.Equal(0, code)
	s.Empty(rep.ConsentWarnings, "a consent far from expiry must not warn")
	s.True(h.provider.hasCallFor(accountUID), "the account must still be checked")
	s.Require().Len(rep.Accounts, 1)
	s.Nil(rep.Accounts[0].Unchecked)
}

// TestConsentExpiringSoonWarnsButStillChecksTheAccount covers the second row of the consent table:
// a ValidUntil 5 days away with consent_warn_days 7 produces exactly one ConsentWarning with
// DaysLeft 5, the account is still checked, the exit code is unaffected by the warning alone, and
// the digest carries the exact "run: firefly-jar auth <bank>" line contracts/digest.md pins.
func (s *ConsentSuite) TestConsentExpiringSoonWarnsButStillChecksTheAccount() {
	const (
		bankKey    = "expiringbank"
		sessionID  = "00000000-0000-0000-0000-0000000001a1"
		accountUID = "00000000-0000-0000-0000-000000000111"
		iban       = "LT000000000000000111"
	)

	validUntil := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

	h := s.newHarness(
		map[string]config.Bank{bankKey: {Name: "Expiring Bank", Country: "LT", Display: "Expiring Bank"}},
		map[string]state.Session{
			bankKey: s.session(sessionID, validUntil, s.account(accountUID, "hash-expiring", iban)),
		},
		[]consentFireflyAccount{{id: "1", iban: iban, currency: "EUR", name: "Main Account"}},
		7,
	)
	h.provider.scripts[accountUID] = consentAccountScript{}

	rep, code := h.run(s.T().Context())

	s.Equal(0, code, "an expiring-soon consent must not change the exit code by itself")
	s.Require().Len(rep.ConsentWarnings, 1)
	s.Equal(bankKey, rep.ConsentWarnings[0].BankKey)
	s.Equal(5, rep.ConsentWarnings[0].DaysLeft)
	s.True(validUntil.Equal(rep.ConsentWarnings[0].ValidUntil), "ValidUntil is carried through unchanged")
	s.True(h.provider.hasCallFor(accountUID), "an expiring consent still gets the account checked")

	s.Require().Len(h.notifier.digests, 1)
	text := h.notifier.digests[0].Text()
	s.Contains(text, "⏰ Consent")
	s.Contains(text, "- "+bankKey+": consent expires 2026-09-27 (5 days) — run: firefly-jar auth "+bankKey)
}

// TestConsentExpiredSessionUnchecksEveryAccountWithoutCallingTheProvider covers the third row of
// the consent table: now at or past ValidUntil unchecks every one of the bank's accounts with
// ConsentExpired, the provider is never called for that bank at all, and the run exits 2.
func (s *ConsentSuite) TestConsentExpiredSessionUnchecksEveryAccountWithoutCallingTheProvider() {
	const (
		bankKey   = "expiredbank"
		sessionID = "00000000-0000-0000-0000-0000000001a2"
		uid1      = "00000000-0000-0000-0000-000000000120"
		uid2      = "00000000-0000-0000-0000-000000000121"
		iban1     = "LT000000000000000120"
		iban2     = "LT000000000000000121"
	)

	h := s.newHarness(
		map[string]config.Bank{bankKey: {Name: "Expired Bank", Country: "LT", Display: "Expired Bank"}},
		map[string]state.Session{
			bankKey: s.session(sessionID, time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC),
				s.account(uid1, "hash-exp-1", iban1), s.account(uid2, "hash-exp-2", iban2)),
		},
		[]consentFireflyAccount{
			{id: "1", iban: iban1, currency: "EUR", name: "Main Account"},
			{id: "2", iban: iban2, currency: "EUR", name: "Savings Account"},
		},
		7,
	)

	rep, code := h.run(s.T().Context())

	s.Equal(2, code)
	s.Empty(h.provider.recorded(), "an already-expired session must cost no bank call")
	s.Require().Len(rep.Accounts, 2)

	for i := range rep.Accounts {
		a := &rep.Accounts[i]
		s.Require().NotNil(a.Unchecked)
		s.Equal(report.ConsentExpired, a.Unchecked.Code)
	}
}

// TestConsentExactlyAtWarnBoundaryWarnsWithDaysLeftSeven pins the closed lower edge of the
// consent-table's EXPIRING row (data-model.md: `ValidUntil − warn_days ≤ now < ValidUntil`): now
// exactly equal to ValidUntil − warn_days is already EXPIRING, not OK, so it still warns (DaysLeft
// 7) and the account is still checked.
func (s *ConsentSuite) TestConsentExactlyAtWarnBoundaryWarnsWithDaysLeftSeven() {
	const (
		bankKey    = "boundarywarnbank"
		sessionID  = "00000000-0000-0000-0000-0000000001a8"
		accountUID = "00000000-0000-0000-0000-000000000150"
		iban       = "LT000000000000000150"
	)

	now := time.Date(2026, 9, 22, 10, 0, 0, 0, s.vilnius)
	validUntil := now.Add(7 * 24 * time.Hour) // ValidUntil − warn_days == now, exactly.

	h := s.newHarness(
		map[string]config.Bank{bankKey: {Name: "Boundary Warn Bank", Country: "LT", Display: "Boundary Warn Bank"}},
		map[string]state.Session{
			bankKey: s.session(sessionID, validUntil, s.account(accountUID, "hash-boundary-warn", iban)),
		},
		[]consentFireflyAccount{{id: "1", iban: iban, currency: "EUR", name: "Main Account"}},
		7,
	)
	h.provider.scripts[accountUID] = consentAccountScript{}

	rep, code := h.run(s.T().Context())

	s.Equal(0, code, "a boundary warning must not change the exit code by itself")
	s.Require().Len(rep.ConsentWarnings, 1, "the closed lower edge of EXPIRING must still warn")
	s.Equal(7, rep.ConsentWarnings[0].DaysLeft)
	s.True(h.provider.hasCallFor(accountUID), "an exactly-at-boundary consent still gets the account checked")
}

// TestConsentExactlyAtValidUntilIsExpired pins the consent-table's EXPIRED row boundary (now ≥
// ValidUntil): now exactly equal to ValidUntil is already EXPIRED, so the account is unchecked
// without ever being called and the run exits 2.
func (s *ConsentSuite) TestConsentExactlyAtValidUntilIsExpired() {
	const (
		bankKey    = "boundaryexpiredbank"
		sessionID  = "00000000-0000-0000-0000-0000000001a9"
		accountUID = "00000000-0000-0000-0000-000000000151"
		iban       = "LT000000000000000151"
	)

	now := time.Date(2026, 9, 22, 10, 0, 0, 0, s.vilnius)

	h := s.newHarness(
		map[string]config.Bank{
			bankKey: {Name: "Boundary Expired Bank", Country: "LT", Display: "Boundary Expired Bank"},
		},
		map[string]state.Session{
			bankKey: s.session(sessionID, now, s.account(accountUID, "hash-boundary-expired", iban)),
		},
		[]consentFireflyAccount{{id: "1", iban: iban, currency: "EUR", name: "Main Account"}},
		7,
	)

	rep, code := h.run(s.T().Context())

	s.Equal(2, code)
	s.False(h.provider.hasCallFor(accountUID), "now == ValidUntil is already EXPIRED, so the account is never called")
	s.Require().Len(rep.Accounts, 1)
	s.Require().NotNil(rep.Accounts[0].Unchecked)
	s.Equal(report.ConsentExpired, rep.Accounts[0].Unchecked.Code)
}

// TestConsentExpiringLaterTodayWarnsWithDaysLeftZero covers DaysLeft's calendar-date rule: a
// ValidUntil later the same local day as now is zero whole days away, still inside consent_warn_days
// 7, so it warns with DaysLeft 0 and the digest reads "(0 days)" (contracts/digest.md).
func (s *ConsentSuite) TestConsentExpiringLaterTodayWarnsWithDaysLeftZero() {
	const (
		bankKey    = "latertodaybank"
		sessionID  = "00000000-0000-0000-0000-0000000001aa"
		accountUID = "00000000-0000-0000-0000-000000000152"
		iban       = "LT000000000000000152"
	)

	validUntil := time.Date(2026, 9, 22, 18, 0, 0, 0, s.vilnius)

	h := s.newHarness(
		map[string]config.Bank{bankKey: {Name: "Later Today Bank", Country: "LT", Display: "Later Today Bank"}},
		map[string]state.Session{
			bankKey: s.session(sessionID, validUntil, s.account(accountUID, "hash-later-today", iban)),
		},
		[]consentFireflyAccount{{id: "1", iban: iban, currency: "EUR", name: "Main Account"}},
		7,
	)
	h.provider.scripts[accountUID] = consentAccountScript{}

	rep, code := h.run(s.T().Context())

	s.Equal(0, code)
	s.Require().Len(rep.ConsentWarnings, 1)
	s.Equal(0, rep.ConsentWarnings[0].DaysLeft, "ValidUntil later the same calendar day is zero whole days left")
	s.True(h.provider.hasCallFor(accountUID))

	s.Require().Len(h.notifier.digests, 1)
	s.Contains(h.notifier.digests[0].Text(),
		"- "+bankKey+": consent expires 2026-09-22 (0 days) — run: firefly-jar auth "+bankKey)
}

// TestConsentOneSecondBeforeWarnBoundaryIsOK pins the open upper edge of the consent-table's OK row
// (now < ValidUntil − warn_days): one second short of the warn window must not warn yet.
func (s *ConsentSuite) TestConsentOneSecondBeforeWarnBoundaryIsOK() {
	const (
		bankKey    = "justoutsidewarnbank"
		sessionID  = "00000000-0000-0000-0000-0000000001ab"
		accountUID = "00000000-0000-0000-0000-000000000153"
		iban       = "LT000000000000000153"
	)

	now := time.Date(2026, 9, 22, 10, 0, 0, 0, s.vilnius)
	validUntil := now.Add(7*24*time.Hour + time.Second) // now is one second before ValidUntil − warn_days.

	h := s.newHarness(
		map[string]config.Bank{
			bankKey: {Name: "Just Outside Warn Bank", Country: "LT", Display: "Just Outside Warn Bank"},
		},
		map[string]state.Session{
			bankKey: s.session(sessionID, validUntil, s.account(accountUID, "hash-just-outside", iban)),
		},
		[]consentFireflyAccount{{id: "1", iban: iban, currency: "EUR", name: "Main Account"}},
		7,
	)
	h.provider.scripts[accountUID] = consentAccountScript{}

	rep, code := h.run(s.T().Context())

	s.Equal(0, code)
	s.Empty(rep.ConsentWarnings, "one second short of the warn window must not warn yet")
	s.True(h.provider.hasCallFor(accountUID))
}

// TestProviderConsentErrorMidRunUnchecksTheRestOfThatBankOnly covers the fourth row of the consent
// table: the provider reporting ErrConsentExpired or ErrConsentRevoked on the first of a bank's two
// accounts unchecks the rest of that bank's accounts with the matching code without calling the
// provider again for them, while a second, unrelated bank is still checked in full.
func (s *ConsentSuite) TestProviderConsentErrorMidRunUnchecksTheRestOfThatBankOnly() {
	cases := []struct {
		name string
		err  error
		code report.UncheckedCode
	}{
		{name: "expired", err: bank.ErrConsentExpired, code: report.ConsentExpired},
		{name: "revoked", err: bank.ErrConsentRevoked, code: report.ConsentRevoked},
	}

	for _, tc := range cases {
		s.Run(tc.name, func() {
			const (
				bankA = "midrunbank"
				bankB = "otherbank"

				sessionA = "00000000-0000-0000-0000-0000000001a3"
				sessionB = "00000000-0000-0000-0000-0000000001a4"

				uidA1 = "00000000-0000-0000-0000-000000000130"
				uidA2 = "00000000-0000-0000-0000-000000000131"
				uidB1 = "00000000-0000-0000-0000-000000000132"

				ibanA1 = "LT000000000000000130"
				ibanA2 = "LT000000000000000131"
				ibanB1 = "LT000000000000000132"
			)

			farFuture := time.Date(2027, 3, 20, 0, 0, 0, 0, time.UTC)

			h := s.newHarness(
				map[string]config.Bank{
					bankA: {Name: "Mid-run Bank", Country: "LT", Display: "Mid-run Bank"},
					bankB: {Name: "Other Bank", Country: "LT", Display: "Other Bank"},
				},
				map[string]state.Session{
					bankA: s.session(sessionA, farFuture,
						s.account(uidA1, "hash-mid-1", ibanA1), s.account(uidA2, "hash-mid-2", ibanA2)),
					bankB: s.session(sessionB, farFuture, s.account(uidB1, "hash-other-1", ibanB1)),
				},
				[]consentFireflyAccount{
					{id: "1", iban: ibanA1, currency: "EUR", name: "Mid-run Main"},
					{id: "2", iban: ibanA2, currency: "EUR", name: "Mid-run Savings"},
					{id: "3", iban: ibanB1, currency: "EUR", name: "Other Main"},
				},
				7,
			)
			h.provider.scripts[uidA1] = consentAccountScript{err: tc.err}
			h.provider.scripts[uidB1] = consentAccountScript{}

			rep, code := h.run(s.T().Context())

			s.Equal(2, code)
			s.True(h.provider.hasCallFor(uidA1), "the failing account is called once")
			s.False(h.provider.hasCallFor(uidA2),
				"the provider must not be called again for midrunbank once its consent error is seen")
			s.True(h.provider.hasCallFor(uidB1), "an unrelated bank must still be checked")

			byUID := s.accountsByUID(rep)

			s.Require().Contains(byUID, uidA1)
			s.Require().NotNil(byUID[uidA1].Unchecked)
			s.Equal(tc.code, byUID[uidA1].Unchecked.Code)

			s.Require().Contains(byUID, uidA2)
			s.Require().NotNil(byUID[uidA2].Unchecked)
			s.Equal(tc.code, byUID[uidA2].Unchecked.Code)

			s.Require().Contains(byUID, uidB1)
			s.Nil(byUID[uidB1].Unchecked, "otherbank's account must be fully checked")
		})
	}
}

// TestConfiguredBankWithNoSessionIsARunLevelProblem covers the "no session in state" row of the
// consent table: the bank's accounts are unknown, so it is a run-level Problem, never a per-account
// Unchecked, and the run exits 2.
func (s *ConsentSuite) TestConfiguredBankWithNoSessionIsARunLevelProblem() {
	const bankKey = "unauthorizedbank"

	h := s.newHarness(
		map[string]config.Bank{bankKey: {Name: "Unauthorized Bank", Country: "LT", Display: "Unauthorized Bank"}},
		map[string]state.Session{},
		nil,
		7,
	)

	rep, code := h.run(s.T().Context())

	s.Equal(2, code)
	s.Empty(h.provider.recorded(), "an unauthorized bank costs no bank call")
	s.Empty(rep.Accounts, "an unauthorized bank has no accounts to check, so no AccountResult at all")
	s.Require().Len(rep.Problems, 1)
	s.Equal(bankKey, rep.Problems[0].Scope)
	s.Equal("not authorized — run: firefly-jar auth "+bankKey, rep.Problems[0].Reason)

	s.Require().Len(h.notifier.digests, 1)
	s.Contains(h.notifier.digests[0].Text(), "- "+bankKey+": not authorized — run: firefly-jar auth "+bankKey)
}

// TestConfiguredBankWithEmptySessionAccountsIsARunLevelProblem covers the T063 ruling (fj-xwu.4.7,
// fj-xwu.4.6): a configured bank whose saved session covers zero accounts is a run-level Problem,
// never silently skipped, the run exits 2, and the hint uses the em dash like every other
// "run: firefly-jar auth <bank>" hint (fj-xwu.4.6 comment 201).
func (s *ConsentSuite) TestConfiguredBankWithEmptySessionAccountsIsARunLevelProblem() {
	const (
		bankKey   = "noaccountsbank"
		sessionID = "00000000-0000-0000-0000-0000000001a5"
	)

	h := s.newHarness(
		map[string]config.Bank{bankKey: {Name: "No Accounts Bank", Country: "LT", Display: "No Accounts Bank"}},
		map[string]state.Session{
			bankKey: s.session(sessionID, time.Date(2027, 3, 20, 0, 0, 0, 0, time.UTC)),
		},
		nil,
		7,
	)

	rep, code := h.run(s.T().Context())

	s.Equal(2, code)
	s.Empty(h.provider.recorded(), "a session with no accounts costs no bank call")
	s.Empty(rep.Accounts)
	s.Require().Len(rep.Problems, 1)
	s.Equal(bankKey, rep.Problems[0].Scope)
	s.Equal("no accounts in the saved session — run: firefly-jar auth "+bankKey, rep.Problems[0].Reason)
}

// TestStateSessionForABankNoLongerConfiguredWarnsAndIsIgnored covers the last row of the consent
// table: a state session for a bank no longer under banks: produces one WARN on stderr and nothing
// else, while every configured bank is checked exactly as if the stale session were not there.
func (s *ConsentSuite) TestStateSessionForABankNoLongerConfiguredWarnsAndIsIgnored() {
	const (
		configuredKey = "keptbank"
		staleKey      = "removedbank"

		sessionID      = "00000000-0000-0000-0000-0000000001a6"
		staleSessionID = "00000000-0000-0000-0000-0000000001a7"

		accountUID = "00000000-0000-0000-0000-000000000140"
		staleUID   = "00000000-0000-0000-0000-000000000141"

		iban      = "LT000000000000000140"
		staleIBAN = "LT000000000000000141"
	)

	farFuture := time.Date(2027, 3, 20, 0, 0, 0, 0, time.UTC)

	h := s.newHarness(
		map[string]config.Bank{configuredKey: {Name: "Kept Bank", Country: "LT", Display: "Kept Bank"}},
		map[string]state.Session{
			configuredKey: s.session(sessionID, farFuture, s.account(accountUID, "hash-kept", iban)),
			staleKey:      s.session(staleSessionID, farFuture, s.account(staleUID, "hash-stale", staleIBAN)),
		},
		[]consentFireflyAccount{{id: "1", iban: iban, currency: "EUR", name: "Main Account"}},
		7,
	)
	h.provider.scripts[accountUID] = consentAccountScript{}

	rep, code := h.run(s.T().Context())

	s.Equal(0, code)
	s.Empty(rep.Problems, "a stale session is ignored, not reported as a problem")
	s.Contains(h.stderr.String(), "WARN", "a stale session logs a warning, not silence")
	s.Contains(h.stderr.String(), staleKey, "the warning names the bank the stale session belongs to")

	calls := h.provider.recorded()
	s.Require().Len(calls, 1, "only the configured bank's account is ever checked")
	s.Equal(accountUID, calls[0].account.UID)
	s.False(h.provider.hasCallFor(staleUID), "the stale session's account is never reachable, so it is never called")
}

// newHarness wires one consent case: banks, sessions, the Firefly III accounts the fake serves and
// consent_warn_days, over the fixed clock (2026-09-22 10:00 Europe/Vilnius).
func (s *ConsentSuite) newHarness(
	banks map[string]config.Bank, sessions map[string]state.Session,
	ffAccounts []consentFireflyAccount, warnDays int,
) *consentHarness {
	s.T().Helper()

	events := &eventLog{}
	ff := &fakeFirefly{events: events, accountsBody: s.accountsBody(ffAccounts...)}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/accounts", ff.serveAccounts)
	mux.HandleFunc("GET /api/v1/accounts/{id}/transactions", ff.serveTransactions)

	ff.srv = httptest.NewServer(mux)
	s.T().Cleanup(ff.srv.Close)

	return &consentHarness{
		cfg: &config.Config{
			Timezone:          "Europe/Vilnius",
			WindowDays:        30,
			DateToleranceDays: 3,
			ConsentWarnDays:   warnDays,
			LogLevel:          "debug",
			Firefly:           config.Firefly{URL: ff.srv.URL + "/api/v1"},
			Banks:             banks,
			Location:          s.vilnius,
		},
		st:       &state.State{Version: state.CurrentVersion, Sessions: sessions},
		events:   events,
		provider: &consentProvider{events: events, scripts: map[string]consentAccountScript{}},
		firefly:  ff,
		notifier: &recordingNotifier{},
		fileLog:  &bytes.Buffer{},
		stderr:   &bytes.Buffer{},
		stdout:   &bytes.Buffer{},
		clock:    time.Date(2026, 9, 22, 10, 0, 0, 0, s.vilnius),
	}
}

// session builds one state.Session with the given id, expiry and accounts.
func (s *ConsentSuite) session(sessionID string, validUntil time.Time, accounts ...state.Account) state.Session {
	return state.Session{
		Provider:     "enablebanking",
		SessionID:    sessionID,
		ValidUntil:   validUntil,
		AuthorizedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		Accounts:     accounts,
	}
}

// account builds one EUR state.Account named "Main" identified by uid, hash and iban.
func (s *ConsentSuite) account(uid, hash, iban string) state.Account {
	return state.Account{UID: uid, Hash: hash, IBAN: iban, Currency: "EUR", Name: "Main"}
}

// accountsBody renders accounts as one Firefly III GET /accounts page, the same shape as
// testdata/firefly/accounts_nometa.json.
func (s *ConsentSuite) accountsBody(accounts ...consentFireflyAccount) []byte {
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
func (s *ConsentSuite) accountsByUID(rep report.RunReport) map[string]report.AccountResult {
	byUID := make(map[string]report.AccountResult, len(rep.Accounts))
	for i := range rep.Accounts {
		byUID[rep.Accounts[i].Mapping.Bank.UID] = rep.Accounts[i]
	}

	return byUID
}
