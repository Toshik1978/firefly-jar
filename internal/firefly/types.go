// Package firefly holds the domain types that represent Firefly III accounts and transactions.
// This package is read-only: it lists and retrieves accounts and transactions (FR-001,
// constitution §I), and never creates, updates, or deletes anything in Firefly III. All transport
// requests are GET only.
package firefly

import (
	"github.com/Toshik1978/firefly-jar/internal/civil"
	"github.com/Toshik1978/firefly-jar/internal/domain"
)

// Account represents a Firefly III asset account.
type Account struct {
	// ID is the Firefly III account identifier.
	ID string
	// Name is the account name.
	Name string
	// IBAN is the account's IBAN, normalized to uppercase, or empty if the account has none.
	IBAN string
	// Currency is the ISO 4217 currency code for this account.
	Currency string
	// Role is the account role, for display only (e.g. ccAsset for credit cards).
	Role string
	// DecimalPlaces is the currency's display precision for amounts in this account.
	DecimalPlaces uint8
	// Active reports whether the account is active. A missing value counts as active.
	Active bool
}

// Entry is one Firefly III transaction group as seen from one asset account: the group's
// comparable withdrawal, deposit and transfer splits touching the account, summed and signed
// relative to it (FR-009).
type Entry struct {
	// GroupID is the transaction group identifier.
	GroupID string
	// AccountID is the asset account this entry is relative to.
	AccountID string
	// Date is the YYYY-MM-DD prefix of the first counted split's date as Firefly III renders it,
	// in civil date format. It is never re-converted from timestamps.
	Date civil.Date
	// Amount is the signed sum of the group's comparable splits, in the account's currency:
	// negative where the account is the source, positive where it is the destination.
	Amount domain.Amount
	// Description is kept in memory for tests and debugging only. It is never logged, per
	// constitution §V.
	Description string
}
