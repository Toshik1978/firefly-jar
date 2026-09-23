package reconcile

import (
	"github.com/Toshik1978/firefly-jar/internal/bank"
	"github.com/Toshik1978/firefly-jar/internal/civil"
)

// HintKind says why a Firefly III entry is suggested for a missing bank transaction.
type HintKind int

const (
	// Taken is an entry within tolerance that was paired with another bank transaction.
	Taken HintKind = iota
	// NearMiss is an entry, paired or not, beyond the tolerance but within twice the tolerance.
	NearMiss
)

// Hint names the Firefly III entry a missing transaction was most likely entered as (FR-025a). It
// is informational only: it never changes a match or any count.
type Hint struct {
	GroupID string
	Date    civil.Date
	Kind    HintKind
}

// hint implements step 6 for the missing transaction tx: among the Taken and NearMiss candidates in
// its bucket it picks the nearest one, then the earlier date, then the lower group id, preferring
// Taken at an equal distance. It returns nil when there is no candidate.
func (b buckets) hint(tx *bank.Transaction, tolerance int) *Hint {
	var (
		best     *Hint
		bestDist int
	)

	// The bucket is already ordered by Date and then GroupID, so replacing the best only on a
	// strictly better distance or kind leaves the date and group id tie-breaks to that order.
	for _, s := range b[tx.Amount.Key()] {
		dist := daysApart(tx.Date, s.entry.Date)

		kind, ok := classify(s, dist, tolerance)
		if !ok {
			continue
		}

		if best == nil || dist < bestDist || (dist == bestDist && kind == Taken && best.Kind == NearMiss) {
			best = &Hint{GroupID: s.entry.GroupID, Date: s.entry.Date, Kind: kind}
			bestDist = dist
		}
	}

	return best
}

// classify reports the hint kind of the entry in s at dist days from a missing transaction, and
// false when the entry is no candidate: an unpaired entry within tolerance (which the transaction
// would have claimed) or any entry beyond twice the tolerance.
func classify(s *slot, dist, tolerance int) (HintKind, bool) {
	switch {
	case dist <= tolerance:
		return Taken, s.paired
	case dist <= 2*tolerance:
		return NearMiss, true
	default:
		return NearMiss, false
	}
}
