// Package report turns one run's per-account mappings and reconcile results into the run-wide
// outcome the rest of the tool acts on (FR-024, FR-029, data-model.md "Run outcome"): the process
// exit code, whether a digest is needed at all, and the summary counts the digest header and the
// accounts command report. Nothing in this package talks to a bank, to Firefly III or to a clock;
// it only interprets values other packages already computed.
package report

import (
	"github.com/Toshik1978/firefly-jar/internal/domain"
	"github.com/Toshik1978/firefly-jar/internal/mapping"
	"github.com/Toshik1978/firefly-jar/internal/reconcile"
)

// RunReport is the outcome of one run: the window it covered, every account's mapping and
// reconcile result (or why it went unchecked), any run-level problem, any consent warning, and how
// delivery went.
type RunReport struct {
	Window          domain.Window
	Accounts        []AccountResult
	ConsentWarnings []ConsentWarning
	Problems        []Problem
	Delivery        Delivery
}

// AccountResult is one bank account's outcome: its mapping, its reconcile result, and, when
// reconciliation could not run at all, why (Unchecked is non-nil then and Result does not apply,
// data-model.md "Run outcome").
type AccountResult struct {
	Mapping   mapping.Mapping
	Result    reconcile.Result
	Unchecked *Unchecked
}

// Problem is a run-level failure that is not tied to one account, such as Firefly III being
// unreachable or a configured bank having no session at all (data-model.md "Run outcome").
type Problem struct {
	Scope  string
	Reason string
}

// Summary counts the accounts and transactions of one run, for the digest header and the accounts
// report. An excluded account contributes nothing; an unchecked account counts only toward
// AccountsUnchecked.
type Summary struct {
	AccountsChecked   int
	AccountsUnchecked int
	// AccountsExcluded counts accounts an accounts: rule with exclude: true set aside: never
	// fetched, never in the digest, but counted here so the summary accounts for every account.
	AccountsExcluded int
	Matched          int
	Missing          int
	Deduplicated     int
	Void             int
}

// ExitCode reports the process exit status for this run (FR-029, contracts/cli.md): 2 if any
// account went unchecked, any run-level Problem exists, or delivery was attempted and every
// recipient failed; otherwise 1 if any checked, non-excluded account has a missing transaction;
// otherwise 0.
func (r RunReport) ExitCode() int {
	if r.hasUnchecked() || len(r.Problems) > 0 || (r.Delivery.Attempted > 0 && r.Delivery.Succeeded == 0) {
		return 2
	}

	if r.hasMissing() {
		return 1
	}

	return 0
}

// DigestNeeded reports whether this run has anything worth sending a digest for (FR-024): a
// missing transaction, a consent warning, an unchecked account, or a run-level problem. Delivery
// outcome never factors in, because it is decided only after the digest is built.
func (r RunReport) DigestNeeded() bool {
	return r.hasMissing() || len(r.ConsentWarnings) > 0 || r.hasUnchecked() || len(r.Problems) > 0
}

// Summary counts this run's accounts and transactions (data-model.md "Run outcome"): an excluded
// account adds only to AccountsExcluded, an unchecked account adds only to AccountsUnchecked, and
// every other account adds to AccountsChecked and to its reconcile counts.
func (r RunReport) Summary() Summary {
	var s Summary

	for i := range r.Accounts {
		a := &r.Accounts[i]

		switch {
		case a.Mapping.Status == mapping.Excluded:
			s.AccountsExcluded++
		case a.Unchecked != nil:
			s.AccountsUnchecked++
		default:
			s.AccountsChecked++
			s.Matched += len(a.Result.Matched)
			s.Missing += len(a.Result.Missing)
			s.Deduplicated += a.Result.Deduplicated
			s.Void += a.Result.Void
		}
	}

	return s
}

// hasUnchecked reports whether any account in r could not be checked.
func (r RunReport) hasUnchecked() bool {
	for i := range r.Accounts {
		if r.Accounts[i].Unchecked != nil {
			return true
		}
	}

	return false
}

// hasMissing reports whether any checked, non-excluded account in r has a missing transaction. An
// excluded account's leftover Result never counts, and an unchecked account has none.
func (r RunReport) hasMissing() bool {
	for i := range r.Accounts {
		a := &r.Accounts[i]
		if a.Mapping.Status == mapping.Excluded || a.Unchecked != nil {
			continue
		}

		if len(a.Result.Missing) > 0 {
			return true
		}
	}

	return false
}
