package report

import "time"

// ConsentWarning flags a bank whose consent is approaching expiry, still checked this run
// (data-model.md "Consent lifecycle"): ValidUntil is when it expires and DaysLeft how many whole
// days remain.
type ConsentWarning struct {
	BankKey    string
	ValidUntil time.Time
	DaysLeft   int
}

// DeliveryFailure is one notifier-and-recipient pair that failed. Recipient is already masked, as
// it must be everywhere it leaves the process.
type DeliveryFailure struct {
	Channel   string
	Recipient string
	Reason    string
}

// Delivery is how sending the digest went: Attempted and Succeeded count recipients across every
// notifier, and Failures details each one that did not succeed (FR-028).
type Delivery struct {
	Attempted int
	Succeeded int
	Failures  []DeliveryFailure
}
