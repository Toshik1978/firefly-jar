// Package mapping resolves every bank account the tool sees to a Firefly III asset account, or
// records why it could not (FR-014, FR-015, FR-016, data-model.md "Mapping"). Resolve applies the
// two-step resolution order: an explicit accounts: override or exclude rule first, then the
// automatic IBAN-and-currency match.
package mapping

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
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
// Override; Candidates is set only for Ambiguous. TargetID and Detail are set only when Status is
// Unmapped because an accounts: override named a Firefly III account id nothing has: Detail holds
// the full sentence data-model.md pins ("override target #N not found"), and TargetID holds just
// "N", so a caller that needs the bare id (app/check_account.go, for the digest's shorter wording)
// never has to parse it back out of Detail.
type Mapping struct {
	Bank       bank.Account
	Status     Status
	Firefly    *firefly.Account
	Candidates []firefly.Account
	Detail     string
	TargetID   string
}

// Resolve maps each of banks to a Firefly III asset account in ff, one Mapping per bank account in
// input order, applying the resolution order from data-model.md "Mapping": the first matching
// accounts: rule in rules (override or exclude), then, for any account no rule matched, the
// automatic IBAN-and-currency step.
func Resolve(banks []bank.Account, ff []firefly.Account, rules []config.AccountRule) []Mapping {
	mappings := make([]Mapping, 0, len(banks))

	for _, acct := range banks {
		if m, ok := resolveOverride(acct, ff, rules); ok {
			mappings = append(mappings, m)

			continue
		}

		mappings = append(mappings, resolveAuto(acct, ff))
	}

	return mappings
}

// resolveOverride applies the override/exclude step of the resolution order (data-model.md
// "Mapping" step 1, FR-015) to one bank account: the first rule in rules matching acct's bank key
// and either its hash or its (normalized) IBAN, optionally narrowed by currency, wins. ok is false
// when no rule matches, so the caller falls through to automatic resolution.
func resolveOverride(acct bank.Account, ff []firefly.Account, rules []config.AccountRule) (Mapping, bool) {
	rule, ok := matchingRule(acct, rules)
	if !ok {
		return Mapping{}, false
	}

	if rule.Exclude {
		return Mapping{Bank: acct, Status: Excluded}, true
	}

	for _, f := range ff {
		if f.ID != rule.FireflyAccountID {
			continue
		}

		// Copy the matched account so Firefly points at its own value, never into ff. An override
		// may target an inactive account (data-model.md "Mapping").
		matched := f

		return Mapping{Bank: acct, Status: Override, Firefly: &matched}, true
	}

	return Mapping{
		Bank:     acct,
		Status:   Unmapped,
		Detail:   fmt.Sprintf("override target #%s not found", rule.FireflyAccountID),
		TargetID: rule.FireflyAccountID,
	}, true
}

// matchingRule returns the first rule in rules for acct's bank key whose hash or (optionally
// currency-narrowed) IBAN matches acct, and whether one was found.
func matchingRule(acct bank.Account, rules []config.AccountRule) (config.AccountRule, bool) {
	iban := normalizeIBAN(acct.IBAN)

	for _, rule := range rules {
		if rule.Bank != acct.BankKey {
			continue
		}

		if rule.Hash != "" && rule.Hash == acct.Hash {
			return rule, true
		}

		if rule.IBAN != "" && normalizeIBAN(rule.IBAN) == iban &&
			(rule.Currency == "" || rule.Currency == acct.Currency) {
			return rule, true
		}
	}

	return config.AccountRule{}, false
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

	slices.SortFunc(candidates, func(a, b firefly.Account) int { return compareAccountID(a.ID, b.ID) })

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

// compareAccountID orders two Firefly III account ids numerically when both parse as integers, so
// id "10" never sorts before id "9" (carry-over from the T038 review), and falls back to a plain
// string comparison otherwise, since Firefly III account ids are not guaranteed numeric.
func compareAccountID(a, b string) int {
	ai, aErr := strconv.Atoi(a)
	bi, bErr := strconv.Atoi(b)

	if aErr == nil && bErr == nil {
		return cmp.Compare(ai, bi)
	}

	return cmp.Compare(a, b)
}
