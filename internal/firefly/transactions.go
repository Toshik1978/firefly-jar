package firefly

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"github.com/Toshik1978/firefly-jar/internal/civil"
	"github.com/Toshik1978/firefly-jar/internal/domain"
)

// datePrefixLen is the length of the YYYY-MM-DD prefix of a Firefly III split date.
const datePrefixLen = len("2006-01-02")

// groupSplits collects one group's splits across pages, deduplicated by transaction_journal_id.
type groupSplits struct {
	id       string
	title    string
	splits   []splitJSON
	journals map[string]struct{}
}

// ListAccountTransactions returns the Firefly III transaction groups touching accountID dated
// start through end inclusive, one Entry per group with its comparable splits summed and signed
// relative to the account (FR-006, FR-006a, FR-009, research R8). currency is the account's
// currency. Entries come back in the order their groups first appear in the listing. A group with
// no withdrawal, deposit or transfer split comparable in currency produces no entry.
func (c *Client) ListAccountTransactions(
	ctx context.Context, accountID, currency string, start, end civil.Date,
) ([]Entry, error) {
	q := url.Values{
		"start": {start.String()},
		"end":   {end.String()},
		"types": {"withdrawal,deposit,transfer"},
		"type":  {"default"},
		"limit": {"500"},
	}

	var (
		order  []*groupSplits
		groups = map[string]*groupSplits{}
	)

	path := "accounts/" + url.PathEscape(accountID) + "/transactions"

	err := fetchPages(ctx, c, path, q, func(page []groupResource) {
		for i := range page {
			order = mergeGroup(order, groups, &page[i])
		}
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("account #%s not found: %w", accountID, err)
		}

		return nil, fmt.Errorf("list transactions of account #%s: %w", accountID, err)
	}

	entries := make([]Entry, 0, len(order))

	for _, group := range order {
		entry, ok, err := c.toEntry(group, accountID, currency)
		if err != nil {
			return nil, fmt.Errorf("account #%s: %w", accountID, err)
		}

		if ok {
			entries = append(entries, entry)
		}
	}

	return entries, nil
}

// toEntry folds one group's splits into a single Entry relative to accountID. ok is false when no
// split of the group counts: an excluded type, a split not touching the account, or one not
// comparable in currency.
func (c *Client) toEntry(group *groupSplits, accountID, currency string) (Entry, bool, error) {
	var (
		entry   Entry
		counted bool
	)

	for i := range group.splits {
		split := &group.splits[i]

		amount, ok, err := c.signedAmount(group.id, split, accountID, currency)
		if err != nil {
			return Entry{}, false, err
		}

		if !ok {
			continue
		}

		if counted {
			entry.Amount, err = entry.Amount.Add(amount)
			if err != nil {
				return Entry{}, false, fmt.Errorf("group %s: %w", group.id, err)
			}

			continue
		}

		date, err := splitDate(split.Date)
		if err != nil {
			return Entry{}, false, fmt.Errorf("group %s: %w", group.id, err)
		}

		entry = Entry{
			GroupID:     group.id,
			AccountID:   accountID,
			Date:        date,
			Amount:      amount,
			Description: groupDescription(group, split),
		}
		counted = true
	}

	return entry, counted, nil
}

// signedAmount returns split's amount in currency, negative when the account is the source and
// positive when it is the destination. ok is false for a split that does not count: a type other
// than withdrawal, deposit or transfer, a split not touching the account, or one with no amount in
// currency. The last case is logged at DEBUG with the group id, account id, date and raw amount
// only -- never the description (constitution §V) -- since a bank transaction it might have
// matched will surface as missing (research R8).
func (c *Client) signedAmount(
	groupID string, split *splitJSON, accountID, currency string,
) (domain.Amount, bool, error) {
	if !countedType(split.Type) {
		return domain.Amount{}, false, nil
	}

	if split.SourceID != accountID && split.DestinationID != accountID {
		return domain.Amount{}, false, nil
	}

	var raw string

	switch {
	case split.CurrencyCode == currency:
		raw = split.Amount
	case split.ForeignCurrencyCode == currency && split.ForeignAmount != "":
		raw = split.ForeignAmount
	default:
		c.logger.Debug("skip split not comparable in account currency",
			"group_id", groupID,
			"account_id", accountID,
			"date", datePrefix(split.Date),
			"amount", split.Amount,
		)

		return domain.Amount{}, false, nil
	}

	amount, err := domain.ParseAmount(raw, currency)
	if err != nil {
		return domain.Amount{}, false, fmt.Errorf("group %s: %w", groupID, err)
	}

	if split.SourceID == accountID {
		amount = amount.Neg()
	}

	return amount, true, nil
}

// mergeGroup folds res into groups, appending a newly seen group to order and dropping any split
// whose non-empty transaction_journal_id was already seen. The server paginates split rows rather
// than groups, so one group can straddle two pages (research R8).
func mergeGroup(order []*groupSplits, groups map[string]*groupSplits, res *groupResource) []*groupSplits {
	group, seen := groups[res.ID]
	if !seen {
		group = &groupSplits{id: res.ID, title: res.Attributes.GroupTitle, journals: map[string]struct{}{}}
		groups[res.ID] = group
		order = append(order, group)
	}

	for i := range res.Attributes.Transactions {
		split := &res.Attributes.Transactions[i]

		// A split without a journal id cannot be recognized on a later page, so it is always kept:
		// deduplicating on the empty id would silently drop every later id-less split of the group.
		if split.JournalID != "" {
			if _, dup := group.journals[split.JournalID]; dup {
				continue
			}

			group.journals[split.JournalID] = struct{}{}
		}

		group.splits = append(group.splits, *split)
	}

	return order
}

// countedType reports whether a split of type kind can match a bank transaction. Opening
// balances, reconciliations and any type this client does not know never do (FR-006a).
func countedType(kind string) bool {
	switch kind {
	case "withdrawal", "deposit", "transfer":
		return true
	default:
		return false
	}
}

// splitDate parses the YYYY-MM-DD prefix of a split date exactly as Firefly III renders it, in
// its own time zone, without re-converting the offset (FR-006a, research R8).
func splitDate(raw string) (civil.Date, error) {
	if len(raw) < datePrefixLen {
		return civil.Date{}, fmt.Errorf("split date too short: %d characters", len(raw))
	}

	date, err := civil.ParseDate(raw[:datePrefixLen])
	if err != nil {
		return civil.Date{}, fmt.Errorf("split date: %w", err)
	}

	return date, nil
}

// datePrefix returns the YYYY-MM-DD prefix of raw for logging, or raw itself when it is shorter.
func datePrefix(raw string) string {
	if len(raw) < datePrefixLen {
		return raw
	}

	return raw[:datePrefixLen]
}

// groupDescription names the entry for the digest: the group title of a split transaction when
// Firefly III has one, otherwise the description of the group's first counted split.
func groupDescription(group *groupSplits, first *splitJSON) string {
	if group.title != "" {
		return group.title
	}

	return first.Description
}
