package digest

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/Toshik1978/firefly-jar/internal/accountmap"
	"github.com/Toshik1978/firefly-jar/internal/civil"
	"github.com/Toshik1978/firefly-jar/internal/reconcile"
)

// defaultDecimalPlaces formats amounts of an account with no Firefly III account to take the
// precision from.
const defaultDecimalPlaces = 2

// noDescription stands in for a description that is empty, or empty once sanitized.
const noDescription = "(no description)"

// missingLines renders the Missing in Firefly III section body: one heading per checked account
// with at least one missing transaction, in account order, each followed by its lines.
func (rn *renderer) missingLines(accounts []accountView) []string {
	var lines []string

	for _, v := range accounts {
		a := v.result
		if a.Unchecked != nil || len(a.Result.Missing) == 0 {
			continue
		}

		lines = append(lines, rn.heading(a.Mapping, v.maskedID))

		places := uint8(defaultDecimalPlaces)
		if a.Mapping.Firefly != nil {
			places = a.Mapping.Firefly.DecimalPlaces
		}

		sorted := sortedMissing(a.Result.Missing)
		for i := range sorted {
			lines = append(lines, rn.missingLine(&sorted[i], places))
		}
	}

	return lines
}

// heading renders `<bank display> · <masked id> · <account name> (<CUR>)`.
func (rn *renderer) heading(m accountmap.Mapping, masked string) string {
	return fmt.Sprintf("%s · %s · %s (%s)",
		rn.bankDisplay(m.Bank.BankKey), masked, Strip(m.Bank.Name), Strip(m.Bank.Currency))
}

// bankDisplay names a bank the way its heading shows it: the configured display name, else the
// configured name, else the bank key itself.
func (rn *renderer) bankDisplay(key string) string {
	b := rn.banks[key]

	return Strip(cmp.Or(b.Display, b.Name, key))
}

// missingLine renders one missing transaction: date, signed amount at the account's precision,
// currency and description, then the pending and last-reminder flags and the hint, in that order.
func (rn *renderer) missingLine(m *reconcile.Missing, places uint8) string {
	var b strings.Builder

	fmt.Fprintf(&b, "- %s  %s %s  %s", m.Tx.Date, m.Tx.Amount.Format(places), Strip(m.Tx.Amount.Currency),
		rn.description(m.Tx.Description))

	if m.Pending {
		b.WriteString("  ⏳ pending")
	}

	if m.LastReminder {
		b.WriteString("  🔚 last reminder")
	}

	if m.Hint != nil {
		b.WriteString("  " + hintText(m.Hint, m.Tx.Date))
	}

	return b.String()
}

// description runs raw through the free-text pipeline, falling back to a placeholder when nothing
// is left.
func (rn *renderer) description(raw string) string {
	return cmp.Or(rn.text(raw), noDescription)
}

// sortedMissing returns a sorted copy of missing, leaving the report's own slice untouched: date
// descending, then signed amount ascending, then raw description.
func sortedMissing(missing []reconcile.Missing) []reconcile.Missing {
	sorted := slices.Clone(missing)

	slices.SortStableFunc(sorted, func(x, y reconcile.Missing) int {
		return cmp.Or(
			y.Tx.Date.Compare(x.Tx.Date),
			x.Tx.Amount.Value.Cmp(y.Tx.Amount.Value),
			cmp.Compare(x.Tx.Description, y.Tx.Description),
		)
	})

	return sorted
}

// hintText renders the Firefly III entry a missing transaction was probably entered as (FR-025a).
// HintKind has no String of its own, so the wording for each kind lives here with the rest of the
// digest's text.
func hintText(h *reconcile.Hint, txDate civil.Date) string {
	why := "paired with another"
	if h.Kind == reconcile.NearMiss {
		why = pluralDays(abs(h.Date.DaysSince(txDate))) + " apart"
	}

	return fmt.Sprintf("≈ Firefly #%s on %s (%s)", Strip(h.GroupID), h.Date, why)
}

// abs returns the absolute value of n.
func abs(n int) int {
	if n < 0 {
		return -n
	}

	return n
}
