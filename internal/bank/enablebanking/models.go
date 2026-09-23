package enablebanking

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Provider status codes that mapStatus names (research R5); HOLD and OTHR need no constant because
// they fall through to pending with any unknown code. The grouped response shape carries no
// per-entry status, so its booked and pending groups are stamped statusBooked and statusPending.
const (
	statusBooked    = "BOOK"
	statusPending   = "PDNG"
	statusCancelled = "CNCL"
	statusRejected  = "RJCT"
	statusScheduled = "SCHD"
)

// Credit/debit indicators that set an amount's sign (research R5).
const (
	indicatorDebit  = "DBIT"
	indicatorCredit = "CRDT"
)

// transactionsPage is one page of GET /accounts/{uid}/transactions. ContinuationKey is nil or
// empty on the last page.
type transactionsPage struct {
	Transactions    transactionList `json:"transactions"`
	ContinuationKey *string         `json:"continuation_key"`
}

// transactionList holds a page's entries in the order the provider returned them. It decodes both
// the flat array shape and the grouped {"booked":[…],"pending":[…]} shape (research R5).
type transactionList []transaction

// groupedTransactions is the grouped response shape, where the group implies the status.
type groupedTransactions struct {
	Booked  []transaction `json:"booked"`
	Pending []transaction `json:"pending"`
}

// transaction carries only the fields the normalizer reads. transaction_id is deliberately absent:
// it can change between fetches and is never used (research R6).
type transaction struct {
	EntryReference        string            `json:"entry_reference"`
	TransactionAmount     amountAndCurrency `json:"transaction_amount"`
	CreditDebitIndicator  string            `json:"credit_debit_indicator"`
	Status                string            `json:"status"`
	TransactionDate       string            `json:"transaction_date"`
	BookingDate           string            `json:"booking_date"`
	ValueDate             string            `json:"value_date"`
	Creditor              *party            `json:"creditor"`
	Debtor                *party            `json:"debtor"`
	RemittanceInformation []string          `json:"remittance_information"`
}

// amountAndCurrency is a decimal amount kept as its exact string form, never a float.
type amountAndCurrency struct {
	Currency string `json:"currency"`
	Amount   string `json:"amount"`
}

// party is a creditor or debtor; only its name feeds the description.
type party struct {
	Name string `json:"name"`
}

// errorResponse is the provider's error body. Only Error and Message are ever used, and both pass
// through the redactor before reaching an error (research R7).
type errorResponse struct {
	Message string `json:"message"`
	Error   string `json:"error"`
}

// UnmarshalJSON accepts the flat array, the grouped object and null. Grouped entries come back
// booked first, then pending, with the group's status stamped on each.
func (l *transactionList) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)

	switch {
	case bytes.Equal(trimmed, []byte("null")):
		*l = nil

		return nil
	case len(trimmed) > 0 && trimmed[0] == '[':
		var flat []transaction
		if err := json.Unmarshal(trimmed, &flat); err != nil {
			return fmt.Errorf("decode flat transactions: %w", err)
		}

		*l = flat

		return nil
	default:
		var grouped groupedTransactions
		if err := json.Unmarshal(trimmed, &grouped); err != nil {
			return fmt.Errorf("decode grouped transactions: %w", err)
		}

		*l = grouped.flatten()

		return nil
	}
}

// flatten returns the booked entries followed by the pending ones, each stamped with the status
// its group implies.
func (g groupedTransactions) flatten() transactionList {
	out := make(transactionList, 0, len(g.Booked)+len(g.Pending))

	for i := range g.Booked {
		g.Booked[i].Status = statusBooked
	}

	for i := range g.Pending {
		g.Pending[i].Status = statusPending
	}

	out = append(out, g.Booked...)
	out = append(out, g.Pending...)

	return out
}
