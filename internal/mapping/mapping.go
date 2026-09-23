// Package mapping resolves every bank account the tool sees to a Firefly III asset account, or
// records why it could not (FR-014, FR-016, data-model.md "Mapping"). Only the automatic
// IBAN-and-currency step of the resolution order is implemented here; override and exclude rules
// (FR-015) land in Resolve once T066 adds them, at the seam noted there.
package mapping

import (
	"cmp"
	"slices"
	"strings"

	"github.com/Toshik1978/firefly-jar/internal/bank"
	"github.com/Toshik1978/firefly-jar/internal/config"
	"github.com/Toshik1978/firefly-jar/internal/firefly"
)

// Status is the outcome of resolving one bank account to a Firefly III account.
type Status int

const (
	// Auto is a bank account matched to exactly one active Firefly III account by IBAN and
	// currency.
	Auto Status = iota
	// Override is a bank account mapped by an explicit accounts: rule.
	Override
	// Excluded is a bank account an accounts: rule excludes from reconciliation.
	Excluded
	// Ambiguous is a bank account matching more than one active Firefly III account.
	Ambiguous
	// Unmapped is a bank account with no override and no unique automatic match.
	Unmapped
)

// String renders a Status exactly as the accounts command prints it (contracts/cli.md).
func (s Status) String() string {
	switch s {
	case Auto:
		return "auto"
	case Override:
		return "override"
	case Excluded:
		return "excluded"
	case Ambiguous:
		return "ambiguous"
	case Unmapped:
		return "unmapped"
	default:
		return "unknown"
	}
}

// Mapping is the resolution outcome for one bank account. Firefly is set only for Auto and
// Override; Candidates is set only for Ambiguous.
type Mapping struct {
	Bank       bank.Account
	Status     Status
	Firefly    *firefly.Account
	Candidates []firefly.Account
	Detail     string
}

// Resolve maps each of banks to a Firefly III asset account in ff, one Mapping per bank account in
// input order, applying the resolution order from data-model.md "Mapping". rules is accepted so
// callers do not need to change once overrides land, but every rule is currently ignored: T066 adds
// the override/exclude step here, before the automatic one, as data-model.md orders them.
func Resolve(banks []bank.Account, ff []firefly.Account, rules []config.AccountRule) []Mapping {
	_ = rules // seam for T066.

	mappings := make([]Mapping, 0, len(banks))
	for _, acct := range banks {
		mappings = append(mappings, resolveAuto(acct, ff))
	}

	return mappings
}

// resolveAuto applies the automatic IBAN-and-currency step of the resolution order to one bank
// account: active Firefly III asset accounts with the same normalized IBAN and the same currency
// are candidates, exactly one is Auto, more than one is Ambiguous, and none (including a bank
// account with no IBAN) is Unmapped.
func resolveAuto(acct bank.Account, ff []firefly.Account) Mapping {
	iban := normalizeIBAN(acct.IBAN)
	if iban == "" {
		return Mapping{Bank: acct, Status: Unmapped}
	}

	var candidates []firefly.Account

	for _, f := range ff {
		if !f.Active {
			continue
		}

		if normalizeIBAN(f.IBAN) == iban && f.Currency == acct.Currency {
			candidates = append(candidates, f)
		}
	}

	slices.SortFunc(candidates, func(a, b firefly.Account) int { return cmp.Compare(a.ID, b.ID) })

	switch len(candidates) {
	case 0:
		return Mapping{Bank: acct, Status: Unmapped}
	case 1:
		// Copy the matched candidate so Firefly points at its own value, never into ff or
		// candidates.
		matched := candidates[0]

		return Mapping{Bank: acct, Status: Auto, Firefly: &matched}
	default:
		return Mapping{Bank: acct, Status: Ambiguous, Candidates: candidates}
	}
}

// normalizeIBAN uppercases iban and strips spaces, so an IBAN a bank rendered with spaces or in
// lower case still compares equal to Firefly III's already-normalized one (data-model.md
// "Mapping").
func normalizeIBAN(iban string) string {
	return strings.ToUpper(strings.ReplaceAll(iban, " ", ""))
}
