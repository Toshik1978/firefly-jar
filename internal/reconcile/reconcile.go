// Package reconcile is the pure matching core behind constitution §II (never miss a transaction):
// it compares one account's bank transactions with that account's Firefly III entries
// (data-model.md "Reconciliation", FR-007, FR-008, FR-010, FR-011, FR-025a, FR-026).
//
// Invariant: every bank transaction dated in the window is accounted for exactly once, so
// len(inWindow) == len(Matched) + len(Missing) + Deduplicated + Void. Reconcile never drops a
// transaction silently. It performs no I/O, logs nothing, never modifies its inputs, and gives the
// same Result for any ordering of them.
package reconcile

import (
	"cmp"
	"slices"
	"strconv"
	"strings"

	"github.com/Toshik1978/firefly-jar/internal/bank"
	"github.com/Toshik1978/firefly-jar/internal/civil"
	"github.com/Toshik1978/firefly-jar/internal/domain"
	"github.com/Toshik1978/firefly-jar/internal/firefly"
)

// Result is the outcome for one account. Matched and Missing are both ordered by the bank
// transaction's Date, Currency, numeric Value, EntryRef, Description and then Status (Booked
// before Pending), so the digest never depends on the order the provider returned.
type Result struct {
	Matched      []Pair
	Missing      []Missing
	Deduplicated int
	Void         int
}

// Pair is a bank transaction together with the Firefly III entry it was matched to.
type Pair struct {
	Bank    bank.Transaction
	Firefly firefly.Entry
}

// Missing is a bank transaction with no Firefly III counterpart. Pending flags an unsettled
// transaction (FR-005b), LastReminder one on the window's first day that the next run no longer
// sees (FR-026), and Hint, when set, names the entry the owner most likely meant (FR-025a).
type Missing struct {
	Tx           bank.Transaction
	Pending      bool
	LastReminder bool
	Hint         *Hint
}

// Reconcile follows data-model.md "Reconciliation" steps 1-6: it keeps the transactions dated in
// w, sets void ones aside, drops pending copies whose booked twin is also in w, and then pairs each
// remaining transaction greedily, in the Result order, with the earliest unused entry of exactly
// the same signed amount and currency whose date lies within tolerance days. Every transaction left
// unpaired is reported as Missing.
func Reconcile(txs []bank.Transaction, entries []firefly.Entry, tolerance int, w domain.Window) Result {
	var res Result

	active := res.screen(txs, w)
	slices.SortStableFunc(active, compareTx)

	pool := newBuckets(entries)

	var unpaired []int

	for i := range active {
		if s := pool.claim(&active[i], tolerance); s != nil {
			res.Matched = append(res.Matched, Pair{Bank: active[i], Firefly: s.entry})

			continue
		}

		unpaired = append(unpaired, i)
	}

	// Hints are looked up only after every pairing is final, so Taken reflects the whole pass.
	for _, i := range unpaired {
		tx := &active[i]
		res.Missing = append(res.Missing, Missing{
			Tx:           *tx,
			Pending:      tx.Status == bank.Pending,
			LastReminder: w.IsFirstDay(tx.Date),
			Hint:         pool.hint(tx, tolerance),
		})
	}

	return res
}

// screen applies steps 1 and 2 to a copy of txs: it keeps the transactions dated in w, counts and
// sets aside void ones, and counts and drops each pending copy that shares a non-empty EntryRef
// with a booked transaction also in w (research R6). It returns what is left to match.
func (r *Result) screen(txs []bank.Transaction, w domain.Window) []bank.Transaction {
	inWindow := make([]bank.Transaction, 0, len(txs))

	for i := range txs {
		switch {
		case !w.Contains(txs[i].Date):
		case txs[i].Status == bank.Void:
			r.Void++
		default:
			inWindow = append(inWindow, txs[i])
		}
	}

	booked := bookedRefs(inWindow)
	active := make([]bank.Transaction, 0, len(inWindow))

	for i := range inWindow {
		if inWindow[i].Status == bank.Pending && booked[inWindow[i].EntryRef] {
			r.Deduplicated++

			continue
		}

		active = append(active, inWindow[i])
	}

	return active
}

// bookedRefs returns the non-empty EntryRefs of the booked transactions in txs. An empty EntryRef
// never links two transactions, because a provider that omits it says nothing about identity.
func bookedRefs(txs []bank.Transaction) map[string]bool {
	refs := make(map[string]bool)

	for i := range txs {
		if txs[i].Status == bank.Booked && txs[i].EntryRef != "" {
			refs[txs[i].EntryRef] = true
		}
	}

	return refs
}

// compareTx is the total order of bank transactions, used both for greedy pairing and for the
// Result order. Status comes last so that, of a booked and a pending copy nothing else tells apart,
// the booked one is paired and the pending one is reported: the conservative outcome.
func compareTx(a, b bank.Transaction) int {
	return cmp.Or(
		a.Date.Compare(b.Date),
		strings.Compare(a.Amount.Currency, b.Amount.Currency),
		a.Amount.Value.Cmp(b.Amount.Value),
		strings.Compare(a.EntryRef, b.EntryRef),
		strings.Compare(a.Description, b.Description),
		cmp.Compare(a.Status, b.Status),
	)
}

// slot is one Firefly III entry and whether a bank transaction has already claimed it.
type slot struct {
	entry  firefly.Entry
	paired bool
}

// buckets groups the Firefly III entries by domain.Amount.Key (currency plus normalized signed
// value, step 3), each bucket ordered by Date and then GroupID (step 4). Only a transaction with
// exactly the same key can ever match or be hinted to an entry.
type buckets map[string][]*slot

// newBuckets builds the buckets from a copy of every entry, leaving the caller's slice untouched.
func newBuckets(entries []firefly.Entry) buckets {
	b := make(buckets)

	for i := range entries {
		key := entries[i].Amount.Key()
		b[key] = append(b[key], &slot{entry: entries[i]})
	}

	for _, slots := range b {
		slices.SortStableFunc(slots, compareSlot)
	}

	return b
}

// claim pairs tx with the earliest unused entry in its bucket whose date lies within tolerance
// days of it (FR-008), marks that entry used and returns it, or returns nil when there is none.
// The earliest, not the nearest, entry wins so that later transactions keep their chance.
func (b buckets) claim(tx *bank.Transaction, tolerance int) *slot {
	for _, s := range b[tx.Amount.Key()] {
		if !s.paired && daysApart(tx.Date, s.entry.Date) <= tolerance {
			s.paired = true

			return s
		}
	}

	return nil
}

// compareSlot orders the entries of a bucket by Date and then GroupID.
func compareSlot(a, b *slot) int {
	return cmp.Or(a.entry.Date.Compare(b.entry.Date), compareGroupID(a.entry.GroupID, b.entry.GroupID))
}

// compareGroupID compares Firefly III group ids numerically when both are integers, so "99" comes
// before "100" as it does in Firefly III, and lexicographically otherwise.
func compareGroupID(a, b string) int {
	x, errA := strconv.ParseUint(a, 10, 64)
	y, errB := strconv.ParseUint(b, 10, 64)

	if errA == nil && errB == nil {
		return cmp.Compare(x, y)
	}

	return strings.Compare(a, b)
}

// daysApart returns the absolute number of days between a and b.
func daysApart(a, b civil.Date) int {
	d := a.DaysSince(b)
	if d < 0 {
		return -d
	}

	return d
}
