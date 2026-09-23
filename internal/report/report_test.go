// Package report_test exercises the run-outcome rules of internal/report (T041, FR-024, FR-029,
// data-model.md "Run outcome"): how one run's per-account mappings and reconcile results become an
// exit code, a digest decision and a summary. The suite methods build report.RunReport values
// directly; nothing here talks to a bank, to Firefly III or to a real clock.
package report_test

import (
	"time"

	"github.com/stretchr/testify/suite"

	"github.com/Toshik1978/firefly-jar/internal/mapping"
	"github.com/Toshik1978/firefly-jar/internal/reconcile"
	"github.com/Toshik1978/firefly-jar/internal/report"
)

// ReportSuite covers RunReport.ExitCode, RunReport.DigestNeeded, RunReport.Summary and
// UncheckedCode.Text (T041).
type ReportSuite struct {
	suite.Suite
}

// outcomeCase is one table row: a RunReport and its expected exit code and digest decision.
type outcomeCase struct {
	name       string
	report     report.RunReport
	wantExit   int
	wantDigest bool
}

// account returns an AccountResult for a checked account: reconciliation ran and found result,
// and Unchecked is nil.
func account(status mapping.Status, result reconcile.Result) report.AccountResult {
	return report.AccountResult{
		Mapping: mapping.Mapping{Status: status},
		Result:  result,
	}
}

// uncheckedAccount returns an AccountResult for an account reconciliation never ran against,
// because it could not be checked for the given reason.
func uncheckedAccount(code report.UncheckedCode) report.AccountResult {
	return report.AccountResult{
		Mapping:   mapping.Mapping{Status: mapping.Auto},
		Unchecked: &report.Unchecked{Code: code, Detail: "detail"},
	}
}

// excludedAccount returns an AccountResult for an account an accounts: rule excludes from
// reconciliation, carrying a non-zero result: a real run never populates one for an excluded
// account, but a non-zero value here proves ExitCode, DigestNeeded and Summary ignore it rather
// than happening to see zero.
func excludedAccount(result reconcile.Result) report.AccountResult {
	return report.AccountResult{
		Mapping: mapping.Mapping{Status: mapping.Excluded},
		Result:  result,
	}
}

// missingResult returns a reconcile.Result reporting n missing bank transactions and nothing else.
func missingResult(n int) reconcile.Result {
	return reconcile.Result{Missing: make([]reconcile.Missing, n)}
}

// matchedResult returns a reconcile.Result reporting n matched bank transactions and nothing else.
func matchedResult(n int) reconcile.Result {
	return reconcile.Result{Matched: make([]reconcile.Pair, n)}
}

// TestExitCodeAndDigestNeeded covers every case data-model.md "Run outcome" lists for ExitCode and
// DigestNeeded (FR-024, FR-029): both are decided from the same fields, so one table exercises both
// at once.
func (s *ReportSuite) TestExitCodeAndDigestNeeded() {
	cases := s.outcomeCases()
	for i := range cases {
		tc := &cases[i]
		s.Run(tc.name, func() {
			s.Equal(tc.wantExit, tc.report.ExitCode(), "exit code")
			s.Equal(tc.wantDigest, tc.report.DigestNeeded(), "digest needed")
		})
	}
}

func (s *ReportSuite) outcomeCases() []outcomeCase {
	return []outcomeCase{
		{
			name:       "clean run: nothing to report",
			report:     report.RunReport{Accounts: []report.AccountResult{account(mapping.Auto, matchedResult(1))}},
			wantExit:   0,
			wantDigest: false,
		},
		{
			name: "consent warning only: warns but nothing failed",
			report: report.RunReport{
				Accounts: []report.AccountResult{account(mapping.Auto, matchedResult(1))},
				ConsentWarnings: []report.ConsentWarning{
					{BankKey: "sample-bank", ValidUntil: time.Now(), DaysLeft: 5},
				},
			},
			wantExit:   0,
			wantDigest: true,
		},
		{
			name: "missing with everything checked",
			report: report.RunReport{
				Accounts: []report.AccountResult{account(mapping.Auto, missingResult(1))},
			},
			wantExit:   1,
			wantDigest: true,
		},
		{
			name: "unchecked account forces exit 2 even with nothing missing",
			report: report.RunReport{
				Accounts: []report.AccountResult{
					account(mapping.Auto, matchedResult(1)),
					uncheckedAccount(report.RateLimited),
				},
			},
			wantExit:   2,
			wantDigest: true,
		},
		{
			name: "run-level problem forces exit 2 with no accounts at all",
			report: report.RunReport{
				Problems: []report.Problem{{Scope: "firefly", Reason: "unreachable"}},
			},
			wantExit:   2,
			wantDigest: true,
		},
		{
			name: "delivery attempted and every recipient failed forces exit 2",
			report: report.RunReport{
				Accounts: []report.AccountResult{account(mapping.Auto, missingResult(1))},
				Delivery: report.Delivery{
					Attempted: 2,
					Succeeded: 0,
					Failures: []report.DeliveryFailure{
						{Channel: "telegram", Recipient: "***", Reason: "timeout"},
						{Channel: "email", Recipient: "***", Reason: "timeout"},
					},
				},
			},
			wantExit:   2,
			wantDigest: true,
		},
		{
			name: "delivery-only failure forces exit 2 but never the digest decision, made after delivery",
			report: report.RunReport{
				Delivery: report.Delivery{
					Attempted: 2,
					Succeeded: 0,
					Failures: []report.DeliveryFailure{
						{Channel: "telegram", Recipient: "***", Reason: "timeout"},
						{Channel: "email", Recipient: "***", Reason: "timeout"},
					},
				},
			},
			wantExit:   2,
			wantDigest: false,
		},
		{
			name: "unchecked account takes precedence over a missing account",
			report: report.RunReport{
				Accounts: []report.AccountResult{
					account(mapping.Auto, missingResult(1)),
					uncheckedAccount(report.BankError),
				},
			},
			wantExit:   2,
			wantDigest: true,
		},
		{
			name: "run-level problem takes precedence over a missing account",
			report: report.RunReport{
				Accounts: []report.AccountResult{account(mapping.Auto, missingResult(1))},
				Problems: []report.Problem{{Scope: "bank", Reason: "not authorized"}},
			},
			wantExit:   2,
			wantDigest: true,
		},
		{
			name: "partial delivery failure with at least one success stays at exit 1",
			report: report.RunReport{
				Accounts: []report.AccountResult{account(mapping.Auto, missingResult(1))},
				Delivery: report.Delivery{
					Attempted: 2,
					Succeeded: 1,
					Failures:  []report.DeliveryFailure{{Channel: "email", Recipient: "***", Reason: "bounced"}},
				},
			},
			wantExit:   1,
			wantDigest: true,
		},
		{
			name: "an excluded account never affects the exit or the digest decision",
			report: report.RunReport{
				Accounts: []report.AccountResult{excludedAccount(missingResult(1))},
			},
			wantExit:   0,
			wantDigest: false,
		},
	}
}

// TestSummary covers Summary's counting rules across one mixed report: a checked account, an
// excluded account carrying a non-zero result, an unchecked account also carrying a non-zero
// result, and a second checked account. No single field's expected count could pass by coincidence
// of matching the number of accounts (data-model.md "Run outcome").
func (s *ReportSuite) TestSummary() {
	uncheckedWithLeftoverResult := report.AccountResult{
		Mapping: mapping.Mapping{Status: mapping.Auto},
		Result: reconcile.Result{
			Matched:      make([]reconcile.Pair, 9),
			Missing:      make([]reconcile.Missing, 9),
			Deduplicated: 9,
			Void:         9,
		},
		Unchecked: &report.Unchecked{Code: report.RateLimited, Detail: "detail"},
	}

	rpt := report.RunReport{
		Accounts: []report.AccountResult{
			account(mapping.Auto, reconcile.Result{
				Matched:      make([]reconcile.Pair, 2),
				Missing:      make([]reconcile.Missing, 1),
				Deduplicated: 1,
				Void:         1,
			}),
			excludedAccount(reconcile.Result{
				Matched:      make([]reconcile.Pair, 5),
				Missing:      make([]reconcile.Missing, 5),
				Deduplicated: 5,
				Void:         5,
			}),
			uncheckedWithLeftoverResult,
			account(mapping.Auto, matchedResult(1)),
		},
	}

	got := rpt.Summary()

	s.Equal(report.Summary{
		AccountsChecked:   2,
		AccountsUnchecked: 1,
		Matched:           3,
		Missing:           1,
		Deduplicated:      1,
		Void:              1,
	}, got)
}

// TestUncheckedCodeText covers Text for every declared UncheckedCode: every one must render
// non-empty text, and the four the design calls out verbatim must render exactly that text
// (data-model.md "Run outcome"). It also covers the zero value and an out-of-range value, neither
// of which is a declared code: a forgotten Code in a hand-built report.Unchecked{} must never be
// mistaken for ConsentExpired, so both render "unknown".
func (s *ReportSuite) TestUncheckedCodeText() {
	cases := []struct {
		name string
		code report.UncheckedCode
		want string // empty means only require non-empty text, no exact match.
	}{
		{"consent expired", report.ConsentExpired, ""},
		{"consent revoked", report.ConsentRevoked, ""},
		{"rate limited", report.RateLimited, "rate limited"},
		{"bank error", report.BankError, ""},
		{"bank data incomplete", report.BankDataIncomplete, "bank data incomplete"},
		{"firefly error", report.FireflyError, ""},
		{"firefly data incomplete", report.FireflyDataIncomplete, ""},
		{"unmapped", report.Unmapped, "no Firefly III account mapped"},
		{"ambiguous", report.Ambiguous, "ambiguous mapping"},
		{"override target missing", report.OverrideTargetMissing, ""},
		{"zero value is never a valid code", report.UncheckedCode(0), "unknown"},
		{"out of range code", report.UncheckedCode(999), "unknown"},
	}

	for _, tc := range cases {
		s.Run(tc.name, func() {
			got := tc.code.Text()
			s.NotEmpty(got, "every code must render some text")

			if tc.want != "" {
				s.Equal(tc.want, got)
			}
		})
	}
}
