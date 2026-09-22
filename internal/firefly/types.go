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

// Entry represents a Firefly III transaction entry (split).
type Entry struct {
	// GroupID is the transaction group identifier.
	GroupID string
	// AccountID is the asset account this entry is relative to.
	AccountID string
	// Date is the YYYY-MM-DD prefix of the split's date as Firefly III renders it, in civil date
	// format. It is never re-converted from timestamps.
	Date civil.Date
	// Amount is signed relative to the account, in the account's currency. Comparable splits in
	// the group are summed.
	Amount domain.Amount
	// Description is kept in memory for tests and debugging only. It is never logged, per
	// constitution §V.
	Description string
}
