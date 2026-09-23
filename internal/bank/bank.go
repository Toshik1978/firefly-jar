// Package bank abstracts a single bank-data provider (Enable Banking today, constitution §IV):
// read-only transaction fetch (Provider) and the provider-neutral consent lifecycle (Authorizer).
// Nothing in this package, or in anything it returns, ever calls a payment endpoint or sends a
// Psu-* header (account-information consent only).
package bank

import (
	"github.com/Toshik1978/firefly-jar/internal/civil"
	"github.com/Toshik1978/firefly-jar/internal/domain"
)

// Account identifies one bank account a provider returned, keyed by the provider's own identifiers
// rather than by account number: UID changes on every re-authorization, but Hash does not (research
// R4). IBAN is masked (LT12…3456) everywhere it leaves this process (constitution §V).
type Account struct {
	BankKey  string
	UID      string
	Hash     string
	IBAN     string
	Currency string
	Name     string
}

// Status is the settlement state of a bank transaction.
type Status int

const (
	// Booked is a transaction the bank has settled.
	Booked Status = iota
	// Pending is a transaction the bank has authorized but not yet settled.
	Pending
	// Void is a transaction the bank later reversed or cancelled.
	Void
)

// String renders a Status as a lowercase word, so %v and test failure output show "pending" rather
// than a bare number. Neither the digest nor the logs call it: the digest flags pending
// transactions its own way, and no record logs a Status.
func (s Status) String() string {
	switch s {
	case Booked:
		return "booked"
	case Pending:
		return "pending"
	case Void:
		return "void"
	default:
		return "unknown"
	}
}

// Transaction is one bank transaction a Provider fetched. Description is never logged, at any level
// (constitution §V): it appears only in the digest.
type Transaction struct {
	Account     Account
	Date        civil.Date
	Amount      domain.Amount
	Status      Status
	EntryRef    string
	Description string
}
