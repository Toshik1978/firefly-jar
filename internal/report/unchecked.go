package report

// UncheckedCode names why one account could not be reconciled. "Not authorized" (a configured bank
// with no session at all) is never one of these: the bank's accounts are unknown, so it is a
// run-level Problem instead (data-model.md "Run outcome").
type UncheckedCode int

// codeUnset is the zero value of UncheckedCode, deliberately reserved so a forgotten Code in a
// hand-built Unchecked{} is never mistaken for ConsentExpired (mirrors config.Command).
const codeUnset UncheckedCode = iota

const (
	// ConsentExpired is a bank whose consent's ValidUntil has passed.
	ConsentExpired UncheckedCode = iota + 1
	// ConsentRevoked is a bank whose consent the provider reports revoked or closed.
	ConsentRevoked
	// RateLimited is a bank call the provider throttled.
	RateLimited
	// BankError is a bank call that failed for a reason other than rate limiting or consent.
	BankError
	// BankDataIncomplete is a bank response the tool could not parse in full, such as an amount
	// with more significant digits than it can represent exactly.
	BankDataIncomplete
	// FireflyError is a Firefly III call that failed for this account specifically.
	FireflyError
	// FireflyDataIncomplete is a Firefly III response the tool could not parse in full.
	FireflyDataIncomplete
	// Unmapped is a bank account with no override and no unique automatic Firefly III match.
	Unmapped
	// Ambiguous is a bank account matching more than one active Firefly III account.
	Ambiguous
	// OverrideTargetMissing is an accounts: override naming a Firefly III account id that does
	// not exist.
	OverrideTargetMissing
)

// Unchecked is why reconciliation never ran for one account. Detail carries data specific to the
// occurrence, such as the redacted bank error or the candidate Firefly III ids for Ambiguous; Code
// alone renders the reason text shown next to it (data-model.md "Run outcome").
type Unchecked struct {
	Code   UncheckedCode
	Detail string
}

// Text renders c the way the digest and the accounts report show it (contracts/digest.md). The
// unset zero value and any out-of-range code, neither of which the current build ever produces on
// purpose, render "unknown" rather than panicking or, worse, silently reading as ConsentExpired.
func (c UncheckedCode) Text() string {
	texts := [...]string{
		ConsentExpired:        "consent expired",
		ConsentRevoked:        "consent revoked",
		RateLimited:           "rate limited",
		BankError:             "bank error",
		BankDataIncomplete:    "bank data incomplete",
		FireflyError:          "Firefly error",
		FireflyDataIncomplete: "Firefly data incomplete",
		Unmapped:              "no Firefly III account mapped",
		Ambiguous:             "ambiguous mapping",
		OverrideTargetMissing: "override target not found",
	}

	if c <= codeUnset || int(c) >= len(texts) {
		return "unknown"
	}

	return texts[c]
}
