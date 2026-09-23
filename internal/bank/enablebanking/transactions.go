package enablebanking

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"unicode"

	"github.com/Toshik1978/firefly-jar/internal/bank"
	"github.com/Toshik1978/firefly-jar/internal/civil"
	"github.com/Toshik1978/firefly-jar/internal/money"
)

// maxPages is the pagination safety cap per account (research R5): a continuation_key chain this
// long is treated as incomplete data rather than followed forever.
const maxPages = 100

// noDescription is the description of an entry with no counterparty name and no remittance line.
const noDescription = "(no description)"

// Transactions fetches every transaction on acc from the day before from onward, following
// continuation_key across pages, and normalizes each entry (research R5). The provider reads
// dates in UTC, hence the extra day; the caller filters the window in its own time zone. The
// session is implied by the account UID and the bearer token, so sessionID is not sent. Nothing
// is deduplicated here: linking pending to booked is the reconciler's job (research R6).
func (c *Client) Transactions(
	ctx context.Context, _ string, acc bank.Account, from civil.Date,
) ([]bank.Transaction, error) {
	path := "/accounts/" + url.PathEscape(acc.UID) + "/transactions"
	dateFrom := from.AddDays(-1).String()

	var (
		out             []bank.Transaction
		continuationKey string
	)

	for range maxPages {
		query := url.Values{"date_from": {dateFrom}}
		if continuationKey != "" {
			query.Set("continuation_key", continuationKey)
		}

		var page transactionsPage
		if err := c.doRequest(ctx, path, query, &page); err != nil {
			return nil, fmt.Errorf("list transactions: %w", err)
		}

		for i := range page.Transactions {
			tx, err := normalize(acc, &page.Transactions[i])
			if err != nil {
				return nil, fmt.Errorf("list transactions: %w", err)
			}

			out = append(out, tx)
		}

		if page.ContinuationKey == nil || *page.ContinuationKey == "" {
			return out, nil
		}

		continuationKey = *page.ContinuationKey
	}

	return nil, &bank.Error{
		Kind:   bank.ErrDataIncomplete,
		Detail: fmt.Sprintf("list transactions: pagination exceeded %d pages", maxPages),
	}
}

// normalize maps one provider entry to a bank.Transaction (research R5). Errors carry the entry
// reference only, never the description or counterparty.
func normalize(acc bank.Account, raw *transaction) (bank.Transaction, error) {
	date, err := entryDate(raw)
	if err != nil {
		return bank.Transaction{}, err
	}

	amount, err := entryAmount(acc, raw)
	if err != nil {
		return bank.Transaction{}, err
	}

	status := mapStatus(raw.Status)
	if amount.IsZero() {
		// Card verifications and released holds move no money.
		status = bank.Void
	}

	return bank.Transaction{
		Account:     acc,
		Date:        date,
		Amount:      amount,
		Status:      status,
		EntryRef:    raw.EntryReference,
		Description: description(raw, amount),
	}, nil
}

// entryDate applies FR-005a: transaction_date, else booking_date, else value_date. An entry with
// none of them, or with an unparseable one, is incomplete data; the date is never guessed.
func entryDate(raw *transaction) (civil.Date, error) {
	for _, candidate := range []string{raw.TransactionDate, raw.BookingDate, raw.ValueDate} {
		if candidate == "" {
			continue
		}

		date, err := civil.ParseDate(candidate)
		if err != nil {
			return civil.Date{}, &bank.Error{
				Kind:   bank.ErrDataIncomplete,
				Detail: fmt.Sprintf("entry %q has a malformed date", raw.EntryReference),
			}
		}

		return date, nil
	}

	return civil.Date{}, &bank.Error{
		Kind:   bank.ErrDataIncomplete,
		Detail: fmt.Sprintf("entry %q has no transaction, booking or value date", raw.EntryReference),
	}
}

// entryAmount parses the amount exactly and signs it from credit_debit_indicator, because the
// provider's own sign convention is undocumented; a missing indicator falls back to that sign
// (research R5). The currency is the entry's, else the account's.
func entryAmount(acc bank.Account, raw *transaction) (money.Amount, error) {
	currency := raw.TransactionAmount.Currency
	if currency == "" {
		currency = acc.Currency
	}

	amount, err := money.ParseAmount(raw.TransactionAmount.Amount, currency)
	if err != nil {
		return money.Amount{}, &bank.Error{
			Kind:   bank.ErrDataIncomplete,
			Detail: fmt.Sprintf("entry %q has a malformed amount", raw.EntryReference),
		}
	}

	switch raw.CreditDebitIndicator {
	case indicatorDebit:
		return amount.Abs().Neg(), nil
	case indicatorCredit:
		return amount.Abs(), nil
	default:
		return amount, nil
	}
}

// mapStatus applies the R5 status table: PDNG, HOLD and OTHR are pending, and so is an unknown or
// missing code, which errs on the side of reporting the entry rather than dropping it.
func mapStatus(code string) bank.Status {
	switch code {
	case statusBooked:
		return bank.Booked
	case statusCancelled, statusRejected, statusScheduled:
		return bank.Void
	default:
		return bank.Pending
	}
}

// description picks the counterparty name for the entry's direction, else the first remittance
// line, else noDescription. Without an indicator, the signed amount gives the direction.
func description(raw *transaction, amount money.Amount) string {
	counterparty := raw.Creditor
	if raw.CreditDebitIndicator == indicatorCredit || (raw.CreditDebitIndicator == "" && amount.Sign() > 0) {
		counterparty = raw.Debtor
	}

	if counterparty != nil {
		if name := sanitize(counterparty.Name); name != "" {
			return name
		}
	}

	for _, line := range raw.RemittanceInformation {
		if text := sanitize(line); text != "" {
			return text
		}
	}

	return noDescription
}

// sanitize removes control characters, which would otherwise reach a Telegram message or email
// verbatim. Line breaks and tabs become spaces so words on either side stay apart.
func sanitize(s string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			return ' '
		case unicode.IsControl(r):
			return -1
		default:
			return r
		}
	}, s)

	return strings.TrimSpace(cleaned)
}
