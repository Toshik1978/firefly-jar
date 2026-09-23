package reconcile_test

import (
	"math/rand/v2"
	"slices"

	"github.com/stretchr/testify/suite"

	"github.com/Toshik1978/firefly-jar/internal/bank"
	"github.com/Toshik1978/firefly-jar/internal/civil"
	"github.com/Toshik1978/firefly-jar/internal/domain"
	"github.com/Toshik1978/firefly-jar/internal/firefly"
	"github.com/Toshik1978/firefly-jar/internal/reconcile"
)

// defaultTolerance is the configured default (FR-007) and the tolerance the spec scenarios use.
const defaultTolerance = 3

// shuffleRuns is how many fixed-seed shuffles TestDeterminism tries per case.
const shuffleRuns = 8

// ReconcileSuite covers the pure matching core (T039): spec US1 scenarios 1-5, FR-005b, FR-007,
// FR-008, FR-010, FR-011, FR-025a, FR-026 and data-model.md "Reconciliation" steps 1-7.
//
// Every case runs through assertCase, which asserts the FR-011 invariant
// len(inWindow) == len(Matched)+len(Missing)+Deduplicated+Void, with inWindow computed here from
// the case's own input (bank transactions dated inside the window, before deduplication).
//
// Output order contract: Matched and Missing are both ordered by the bank transaction's Date, then
// its Amount (currency code, then numeric value, ascending), then EntryRef, then Description, then
// Status (Booked before Pending). The same total key orders bank transactions inside an amount
// bucket before greedy pairing, so of a booked and a pending copy that nothing else tells apart the
// booked one is paired and the pending one is reported (FR-008). Firefly entries inside a bucket
// are ordered by Date, then GroupID, and that GroupID order also breaks hint ties (FR-025a). Group
// ids compare numerically when both parse as integers ("99" before "100"), otherwise
// lexicographically. Pairing never depends on input order.
//
// Test data identify a bank transaction by its Description (the case label) together with its
// Status, which is unique within a case, and a Firefly entry by its GroupID.
type ReconcileSuite struct {
	suite.Suite

	window domain.Window
}

// wantPair is the expected projection of one reconcile.Pair.
type wantPair struct {
	label   string
	groupID string
}

// wantMissing is the expected projection of one reconcile.Missing.
type wantMissing struct {
	label        string
	pending      bool
	lastReminder bool
	hint         *reconcile.Hint
}

// reconcileCase is one table row: the input of a Reconcile call and its complete expected result.
type reconcileCase struct {
	name         string
	txs          []bank.Transaction
	entries      []firefly.Entry
	tolerance    int
	matched      []wantPair
	missing      []wantMissing
	deduplicated int
	void         int
}

// SetupTest fixes the window every case uses: 30 days ending on 2026-09-23, so From is 2026-08-25.
func (s *ReconcileSuite) SetupTest() {
	s.window = domain.NewWindow(s.date("2026-09-23"), 30)
	s.Require().Equal(s.date("2026-08-25"), s.window.From, "window start the cases rely on")
}

// TestAcceptanceScenarios covers spec US1 acceptance scenarios 1-5.
func (s *ReconcileSuite) TestAcceptanceScenarios() {
	s.runCases(s.scenarioCases())
}

// TestToleranceEdges covers the ± tolerance boundary (FR-007).
func (s *ReconcileSuite) TestToleranceEdges() {
	s.runCases(s.toleranceCases())
}

// TestGreedyOrder covers the deterministic greedy pairing (FR-008).
func (s *ReconcileSuite) TestGreedyOrder() {
	s.runCases(s.greedyCases())
}

// TestCurrencyAndSign covers "same direction, exactly the same amount in the account currency" (FR-007).
func (s *ReconcileSuite) TestCurrencyAndSign() {
	s.runCases(s.currencyAndSignCases())
}

// TestWindowBounds covers data-model step 1: only transactions dated in the window are counted.
func (s *ReconcileSuite) TestWindowBounds() {
	s.runCases(s.windowCases())
}

// TestVoid covers data-model step 1: void transactions are counted and set aside (FR-011).
func (s *ReconcileSuite) TestVoid() {
	s.runCases(s.voidCases())
}

// TestDeduplication covers data-model step 2 (FR-005b, research R6).
func (s *ReconcileSuite) TestDeduplication() {
	s.runCases(s.dedupCases())
}

// TestUnmatchedFireflyEntries covers FR-010: Firefly entries without a bank counterpart are silent.
func (s *ReconcileSuite) TestUnmatchedFireflyEntries() {
	s.runCases(s.fireflyOnlyCases())
}

// TestSplitsAndTransfers covers FR-009 as seen by reconcile: a split group arrives as one summed
// entry, and a transfer arrives signed relative to the account.
func (s *ReconcileSuite) TestSplitsAndTransfers() {
	s.runCases(s.splitAndTransferCases())
}

// TestHints covers the FR-025a hint kinds and bounds (data-model step 6).
func (s *ReconcileSuite) TestHints() {
	s.runCases(s.hintCases())
}

// TestHintTieBreaks covers the FR-025a candidate order: nearest, then earlier date, then lower
// group id. Taken (within tolerance) is always nearer than NearMiss (beyond it), so the two kinds
// never tie.
func (s *ReconcileSuite) TestHintTieBreaks() {
	s.runCases(s.hintTieCases())
}

// TestOutputOrder covers the Matched and Missing output order contract documented on the suite.
func (s *ReconcileSuite) TestOutputOrder() {
	s.runCases(s.orderCases())
}

// TestHintsNeverChangeCounts adds decoy entries that can only ever produce NearMiss hints and
// asserts that matched and missing stay exactly the same, apart from the hints themselves.
func (s *ReconcileSuite) TestHintsNeverChangeCounts() {
	txs := []bank.Transaction{
		s.booked("hc-a", "2026-09-10", "-6.60"),
		s.booked("hc-b", "2026-09-10", "-6.60"),
		s.booked("hc-c", "2026-09-15", "-6.61"),
	}
	base := []firefly.Entry{s.eurEntry("901", "2026-09-11", "-6.60")}
	decoys := []firefly.Entry{
		s.eurEntry("902", "2026-09-20", "-6.61"),
		s.eurEntry("903", "2026-09-05", "-6.60"),
		s.eurEntry("904", "2026-09-09", "-6.61"),
	}

	without := reconcileCase{
		name:      "without decoys",
		txs:       txs,
		entries:   base,
		tolerance: defaultTolerance,
		matched:   []wantPair{{label: "hc-a", groupID: "901"}},
		missing: []wantMissing{
			{label: "hc-b", hint: s.hint(reconcile.Taken, "901", "2026-09-11")},
			{label: "hc-c"},
		},
	}
	with := reconcileCase{
		name:      "with decoys",
		txs:       txs,
		entries:   slices.Concat(base, decoys),
		tolerance: defaultTolerance,
		matched:   []wantPair{{label: "hc-a", groupID: "901"}},
		missing: []wantMissing{
			{label: "hc-b", hint: s.hint(reconcile.Taken, "901", "2026-09-11")},
			{label: "hc-c", hint: s.hint(reconcile.NearMiss, "902", "2026-09-20")},
		},
	}

	var gotWithout, gotWith reconcile.Result

	s.Run(without.name, func() { gotWithout = s.assertCase(&without, without.txs, without.entries) })
	s.Run(with.name, func() { gotWith = s.assertCase(&with, with.txs, with.entries) })

	s.Equal(pairViews(gotWithout.Matched), pairViews(gotWith.Matched), "matched pairs")
	s.Equal(stripHints(missingViews(gotWithout.Missing)), stripHints(missingViews(gotWith.Missing)), "missing")
	s.Equal(gotWithout.Deduplicated, gotWith.Deduplicated, "deduplicated")
	s.Equal(gotWithout.Void, gotWith.Void, "void")
}

// TestDeterminism reruns every case of the suite with its bank transactions and Firefly entries
// shuffled by a fixed-seed PCG, and asserts the identical Result (hints and order included) each
// time.
func (s *ReconcileSuite) TestDeterminism() {
	cases := s.allCases()
	for i := range cases {
		tc := &cases[i]
		s.Run(tc.name, func() {
			baseline := reconcile.Reconcile(slices.Clone(tc.txs), slices.Clone(tc.entries), tc.tolerance, s.window)

			for seed := range uint64(shuffleRuns) {
				rng := rand.New(rand.NewPCG(seed, 0x5eed))

				txs := slices.Clone(tc.txs)
				rng.Shuffle(len(txs), func(i, j int) { txs[i], txs[j] = txs[j], txs[i] })

				entries := slices.Clone(tc.entries)
				rng.Shuffle(len(entries), func(i, j int) { entries[i], entries[j] = entries[j], entries[i] })

				got := s.assertCase(tc, txs, entries)
				s.Equal(baseline, got, "seed %d", seed)
			}
		})
	}
}

// scenarioCases are spec US1 acceptance scenarios 1-5.
func (s *ReconcileSuite) scenarioCases() []reconcileCase {
	return []reconcileCase{
		{
			name:      "US1-1 -12.40 on 09-20 matches the Firefly entry on 09-21",
			txs:       []bank.Transaction{s.booked("s1", "2026-09-20", "-12.40")},
			entries:   []firefly.Entry{s.eurEntry("101", "2026-09-21", "-12.40")},
			tolerance: defaultTolerance,
			matched:   []wantPair{{label: "s1", groupID: "101"}},
		},
		{
			name:      "US1-2 no Firefly entry of the same amount is missing",
			txs:       []bank.Transaction{s.booked("s2", "2026-09-20", "-12.40")},
			entries:   []firefly.Entry{s.eurEntry("102", "2026-09-20", "-21.40")},
			tolerance: defaultTolerance,
			missing:   []wantMissing{{label: "s2"}},
		},
		{
			name: "US1-3 two -5.00 on the same day and one entry leave exactly one missing",
			txs: []bank.Transaction{
				s.booked("coffee-2", "2026-09-15", "-5.00"),
				s.booked("coffee-1", "2026-09-15", "-5.00"),
			},
			entries:   []firefly.Entry{s.eurEntry("103", "2026-09-15", "-5.00")},
			tolerance: defaultTolerance,
			matched:   []wantPair{{label: "coffee-1", groupID: "103"}},
			missing: []wantMissing{
				{label: "coffee-2", hint: s.hint(reconcile.Taken, "103", "2026-09-15")},
			},
		},
		{
			name: "US1-4 an unmatched pending transaction is missing and flagged pending",
			txs: []bank.Transaction{
				s.pending("s4", "2026-09-18", "-30.00"),
				s.pending("s4-entered", "2026-09-19", "-31.00"),
			},
			entries:   []firefly.Entry{s.eurEntry("104", "2026-09-19", "-31.00")},
			tolerance: defaultTolerance,
			matched:   []wantPair{{label: "s4-entered", groupID: "104"}},
			missing:   []wantMissing{{label: "s4", pending: true}},
		},
		{
			name: "US1-5 a missing transaction on window.From is flagged last reminder",
			txs: []bank.Transaction{
				s.booked("lr-first", "2026-08-25", "-8.00"),
				s.booked("lr-second", "2026-08-26", "-8.01"),
				s.pending("lr-pending", "2026-08-25", "-8.02"),
				s.booked("lr-entered", "2026-08-25", "-8.03"),
			},
			entries:   []firefly.Entry{s.eurEntry("105", "2026-08-23", "-8.03")},
			tolerance: defaultTolerance,
			matched:   []wantPair{{label: "lr-entered", groupID: "105"}},
			missing: []wantMissing{
				{label: "lr-pending", pending: true, lastReminder: true},
				{label: "lr-first", lastReminder: true},
				{label: "lr-second"},
			},
		},
	}
}

// toleranceCases pin the ± tolerance boundary in both directions (FR-007).
func (s *ReconcileSuite) toleranceCases() []reconcileCase {
	return []reconcileCase{
		{
			name: "a difference of exactly 3 days matches, before and after",
			txs: []bank.Transaction{
				s.booked("te-after", "2026-09-10", "-3.00"),
				s.booked("te-before", "2026-09-10", "-3.01"),
			},
			entries: []firefly.Entry{
				s.eurEntry("331", "2026-09-13", "-3.00"),
				s.eurEntry("332", "2026-09-07", "-3.01"),
			},
			tolerance: defaultTolerance,
			matched: []wantPair{
				{label: "te-before", groupID: "332"},
				{label: "te-after", groupID: "331"},
			},
		},
		{
			name: "a difference of 4 days does not match, before and after",
			txs: []bank.Transaction{
				s.booked("te-after4", "2026-09-10", "-4.00"),
				s.booked("te-before4", "2026-09-10", "-4.01"),
			},
			entries: []firefly.Entry{
				s.eurEntry("333", "2026-09-14", "-4.00"),
				s.eurEntry("334", "2026-09-06", "-4.01"),
			},
			tolerance: defaultTolerance,
			missing: []wantMissing{
				{label: "te-before4", hint: s.hint(reconcile.NearMiss, "334", "2026-09-06")},
				{label: "te-after4", hint: s.hint(reconcile.NearMiss, "333", "2026-09-14")},
			},
		},
		{
			name: "tolerance 0 matches only the same day and gives no near-miss hint",
			txs: []bank.Transaction{
				s.booked("z-same", "2026-09-10", "-1.00"),
				s.booked("z-next", "2026-09-10", "-2.00"),
			},
			entries: []firefly.Entry{
				s.eurEntry("341", "2026-09-10", "-1.00"),
				s.eurEntry("342", "2026-09-11", "-2.00"),
			},
			tolerance: 0,
			matched:   []wantPair{{label: "z-same", groupID: "341"}},
			missing:   []wantMissing{{label: "z-next"}},
		},
		{
			name:      "an equal amount written with a different scale matches exactly",
			txs:       []bank.Transaction{s.booked("scale", "2026-09-10", "-12.4")},
			entries:   []firefly.Entry{s.eurEntry("351", "2026-09-10", "-12.40")},
			tolerance: defaultTolerance,
			matched:   []wantPair{{label: "scale", groupID: "351"}},
		},
	}
}

// greedyCases pin FR-008: each bank transaction, in date order, takes the earliest unused entry
// within tolerance.
func (s *ReconcileSuite) greedyCases() []reconcileCase {
	return []reconcileCase{
		{
			name: "bank 09-10 and 09-12 with entries 09-11 and 09-13 both match",
			txs: []bank.Transaction{
				s.booked("g-2", "2026-09-12", "-6.00"),
				s.booked("g-1", "2026-09-10", "-6.00"),
			},
			entries: []firefly.Entry{
				s.eurEntry("302", "2026-09-13", "-6.00"),
				s.eurEntry("301", "2026-09-11", "-6.00"),
			},
			tolerance: defaultTolerance,
			matched: []wantPair{
				{label: "g-1", groupID: "301"},
				{label: "g-2", groupID: "302"},
			},
		},
		{
			name: "the earliest unused entry within tolerance wins over the nearest one",
			txs:  []bank.Transaction{s.booked("g-3", "2026-09-20", "-6.50")},
			entries: []firefly.Entry{
				s.eurEntry("312", "2026-09-20", "-6.50"),
				s.eurEntry("311", "2026-09-17", "-6.50"),
			},
			tolerance: defaultTolerance,
			matched:   []wantPair{{label: "g-3", groupID: "311"}},
		},
		{
			name: "of a booked and a pending copy nothing else tells apart, the booked one is matched",
			txs: []bank.Transaction{
				withRef(s.pending("twin", "2026-09-14", "-6.70"), ""),
				withRef(s.booked("twin", "2026-09-14", "-6.70"), ""),
			},
			entries:   []firefly.Entry{s.eurEntry("321", "2026-09-14", "-6.70")},
			tolerance: defaultTolerance,
			matched:   []wantPair{{label: "twin", groupID: "321"}},
			missing: []wantMissing{
				{label: "twin", pending: true, hint: s.hint(reconcile.Taken, "321", "2026-09-14")},
			},
		},
		{
			name: "entries on the same date pair in numeric group id order",
			txs: []bank.Transaction{
				s.booked("gn-b", "2026-09-15", "-6.80"),
				s.booked("gn-a", "2026-09-15", "-6.80"),
			},
			entries: []firefly.Entry{
				s.eurEntry("100", "2026-09-15", "-6.80"),
				s.eurEntry("99", "2026-09-15", "-6.80"),
			},
			tolerance: defaultTolerance,
			matched: []wantPair{
				{label: "gn-a", groupID: "99"},
				{label: "gn-b", groupID: "100"},
			},
		},
	}
}

// currencyAndSignCases pin that the matching key is the signed amount in its currency (FR-007).
func (s *ReconcileSuite) currencyAndSignCases() []reconcileCase {
	return []reconcileCase{
		{
			name:      "a different currency never matches",
			txs:       []bank.Transaction{s.booked("c-eur", "2026-09-10", "-10.00")},
			entries:   []firefly.Entry{s.entry("361", "2026-09-10", "-10.00", "USD")},
			tolerance: defaultTolerance,
			missing:   []wantMissing{{label: "c-eur"}},
		},
		{
			name:      "a different sign never matches",
			txs:       []bank.Transaction{s.booked("c-sign", "2026-09-11", "-11.00")},
			entries:   []firefly.Entry{s.eurEntry("362", "2026-09-11", "11.00")},
			tolerance: defaultTolerance,
			missing:   []wantMissing{{label: "c-sign"}},
		},
		{
			name: "a USD transaction matches the USD entry, not the EUR one",
			txs:  []bank.Transaction{s.tx("c-usd", "2026-09-12", "-10.00", "USD", bank.Booked)},
			entries: []firefly.Entry{
				s.eurEntry("364", "2026-09-12", "-10.00"),
				s.entry("363", "2026-09-13", "-10.00", "USD"),
			},
			tolerance: defaultTolerance,
			matched:   []wantPair{{label: "c-usd", groupID: "363"}},
		},
	}
}

// windowCases pin data-model step 1: a transaction outside the window is in no count and never
// consumes a Firefly entry.
func (s *ReconcileSuite) windowCases() []reconcileCase {
	return []reconcileCase{
		{
			name: "bank transactions outside the window are excluded from every count",
			txs: []bank.Transaction{
				s.booked("ow-before", "2026-08-24", "-2.00"),
				s.booked("ow-in", "2026-08-26", "-2.00"),
				s.booked("ow-after", "2026-09-24", "-2.50"),
				s.voided("ow-void", "2026-08-20", "-3.00"),
				s.pending("ow-pending", "2026-08-24", "-4.00"),
			},
			entries:   []firefly.Entry{s.eurEntry("561", "2026-08-25", "-2.00")},
			tolerance: defaultTolerance,
			matched:   []wantPair{{label: "ow-in", groupID: "561"}},
		},
		{
			name:      "a transaction on window.To is checked and is not a last reminder",
			txs:       []bank.Transaction{s.booked("ow-today", "2026-09-23", "-2.60")},
			tolerance: defaultTolerance,
			missing:   []wantMissing{{label: "ow-today"}},
		},
	}
}

// voidCases pin that Void is counted, never missing, and never consumes a Firefly entry.
func (s *ReconcileSuite) voidCases() []reconcileCase {
	return []reconcileCase{
		{
			name: "void is counted, never missing, and leaves its entry to a booked twin",
			txs: []bank.Transaction{
				s.voided("v-1", "2026-09-12", "-15.00"),
				s.booked("v-2", "2026-09-13", "-15.00"),
				s.voided("v-3", "2026-09-14", "-16.00"),
			},
			entries:   []firefly.Entry{s.eurEntry("571", "2026-09-12", "-15.00")},
			tolerance: defaultTolerance,
			matched:   []wantPair{{label: "v-2", groupID: "571"}},
			void:      2,
		},
		{
			name:      "void on window.From is counted, not reminded",
			txs:       []bank.Transaction{s.voided("v-first", "2026-08-25", "-17.00")},
			tolerance: defaultTolerance,
			void:      1,
		},
	}
}

// dedupCases pin data-model step 2: only a Pending/Booked pair with the same non-empty EntryRef,
// both in the window, is merged into the booked one.
func (s *ReconcileSuite) dedupCases() []reconcileCase {
	return []reconcileCase{
		{
			name: "pending and booked with the same EntryRef keep the booked one",
			txs: []bank.Transaction{
				withRef(s.pending("d-pending", "2026-09-18", "-7.00"), "dup-1"),
				withRef(s.booked("d-booked", "2026-09-19", "-7.00"), "dup-1"),
			},
			entries:      []firefly.Entry{s.eurEntry("581", "2026-09-19", "-7.00")},
			tolerance:    defaultTolerance,
			matched:      []wantPair{{label: "d-booked", groupID: "581"}},
			deduplicated: 1,
		},
		{
			name: "the EntryRef alone decides, even when the booked amount differs",
			txs: []bank.Transaction{
				withRef(s.booked("d2-booked", "2026-09-19", "-7.05"), "dup-2"),
				withRef(s.pending("d2-pending", "2026-09-18", "-7.00"), "dup-2"),
			},
			tolerance:    defaultTolerance,
			missing:      []wantMissing{{label: "d2-booked"}},
			deduplicated: 1,
		},
		{
			name: "a pending copy whose booked twin is outside the window stays",
			txs: []bank.Transaction{
				withRef(s.booked("d3-booked", "2026-08-20", "-9.00"), "dup-3"),
				withRef(s.pending("d3-pending", "2026-08-26", "-9.00"), "dup-3"),
			},
			tolerance: defaultTolerance,
			missing:   []wantMissing{{label: "d3-pending", pending: true}},
		},
		{
			name: "an empty EntryRef never deduplicates",
			txs: []bank.Transaction{
				withRef(s.pending("e-pending", "2026-09-05", "-5.50"), ""),
				withRef(s.booked("e-booked", "2026-09-05", "-5.50"), ""),
			},
			tolerance: defaultTolerance,
			missing: []wantMissing{
				{label: "e-booked"},
				{label: "e-pending", pending: true},
			},
		},
		{
			name: "same date and amount with different EntryRefs are never merged",
			txs: []bank.Transaction{
				s.pending("dc-2", "2026-09-06", "-3.20"),
				s.booked("dc-1", "2026-09-06", "-3.20"),
			},
			tolerance: defaultTolerance,
			missing: []wantMissing{
				{label: "dc-1"},
				{label: "dc-2", pending: true},
			},
		},
		{
			name: "two pending copies sharing one EntryRef with a booked twin are both deduplicated",
			txs: []bank.Transaction{
				withRef(s.pending("d4-pending-2", "2026-09-17", "-7.10"), "dup-4"),
				withRef(s.booked("d4-booked", "2026-09-18", "-7.10"), "dup-4"),
				withRef(s.pending("d4-pending-1", "2026-09-16", "-7.10"), "dup-4"),
			},
			entries:      []firefly.Entry{s.eurEntry("582", "2026-09-18", "-7.10")},
			tolerance:    defaultTolerance,
			matched:      []wantPair{{label: "d4-booked", groupID: "582"}},
			deduplicated: 2,
		},
	}
}

// fireflyOnlyCases pin FR-010: a Firefly entry with no bank counterpart is never reported.
func (s *ReconcileSuite) fireflyOnlyCases() []reconcileCase {
	return []reconcileCase{
		{
			name: "Firefly entries with no bank counterpart add no missing item and no count",
			txs:  []bank.Transaction{s.booked("f-1", "2026-09-10", "-1.00")},
			entries: []firefly.Entry{
				s.eurEntry("402", "2026-09-11", "-99.00"),
				s.eurEntry("401", "2026-09-10", "-1.00"),
				s.eurEntry("403", "2026-09-12", "50.00"),
			},
			tolerance: defaultTolerance,
			matched:   []wantPair{{label: "f-1", groupID: "401"}},
		},
		{
			name: "only Firefly entries give an empty result",
			entries: []firefly.Entry{
				s.eurEntry("404", "2026-09-11", "-1.00"),
				s.eurEntry("405", "2026-09-12", "2.00"),
			},
			tolerance: defaultTolerance,
		},
		{
			name:      "no input at all gives an empty result",
			tolerance: defaultTolerance,
		},
	}
}

// splitAndTransferCases pin that reconcile compares the adapter's summed, account-relative entry.
func (s *ReconcileSuite) splitAndTransferCases() []reconcileCase {
	return []reconcileCase{
		{
			name: "a split group matches by its summed amount, not by a part",
			txs: []bank.Transaction{
				s.booked("sp-whole", "2026-09-10", "-30.00"),
				s.booked("sp-part", "2026-09-11", "-20.00"),
			},
			entries:   []firefly.Entry{s.eurEntry("751", "2026-09-10", "-30.00")},
			tolerance: defaultTolerance,
			matched:   []wantPair{{label: "sp-whole", groupID: "751"}},
			missing:   []wantMissing{{label: "sp-part"}},
		},
		{
			name: "transfers match by their direction relative to the account",
			txs: []bank.Transaction{
				s.booked("tr-out", "2026-09-10", "-100.00"),
				s.booked("tr-in", "2026-09-12", "250.00"),
				s.booked("tr-wrong-way", "2026-09-14", "-40.00"),
			},
			entries: []firefly.Entry{
				s.eurEntry("761", "2026-09-11", "-100.00"),
				s.eurEntry("762", "2026-09-12", "250.00"),
				s.eurEntry("763", "2026-09-14", "40.00"),
			},
			tolerance: defaultTolerance,
			matched: []wantPair{
				{label: "tr-out", groupID: "761"},
				{label: "tr-in", groupID: "762"},
			},
			missing: []wantMissing{{label: "tr-wrong-way"}},
		},
	}
}

// hintCases pin the FR-025a hint kinds and their distance bounds.
func (s *ReconcileSuite) hintCases() []reconcileCase {
	return []reconcileCase{
		{
			name: "Taken: 09-10 matches the 09-13 entry and 09-15 is missing with a hint to it",
			txs: []bank.Transaction{
				s.booked("h-b", "2026-09-15", "-4.50"),
				s.booked("h-a", "2026-09-10", "-4.50"),
			},
			entries:   []firefly.Entry{s.eurEntry("411", "2026-09-13", "-4.50")},
			tolerance: defaultTolerance,
			matched:   []wantPair{{label: "h-a", groupID: "411"}},
			missing: []wantMissing{
				{label: "h-b", hint: s.hint(reconcile.Taken, "411", "2026-09-13")},
			},
		},
		{
			name:      "NearMiss at 5 days (5 <= 2x3)",
			txs:       []bank.Transaction{s.booked("nm5", "2026-09-10", "-9.99")},
			entries:   []firefly.Entry{s.eurEntry("421", "2026-09-15", "-9.99")},
			tolerance: defaultTolerance,
			missing: []wantMissing{
				{label: "nm5", hint: s.hint(reconcile.NearMiss, "421", "2026-09-15")},
			},
		},
		{
			name:      "NearMiss at exactly twice the tolerance, before the transaction",
			txs:       []bank.Transaction{s.booked("nm6", "2026-09-10", "-9.98")},
			entries:   []firefly.Entry{s.eurEntry("422", "2026-09-04", "-9.98")},
			tolerance: defaultTolerance,
			missing: []wantMissing{
				{label: "nm6", hint: s.hint(reconcile.NearMiss, "422", "2026-09-04")},
			},
		},
		{
			name: "no hint at 7 days (7 > 6), after or before",
			txs: []bank.Transaction{
				s.booked("far-a", "2026-09-10", "-9.97"),
				s.booked("far-b", "2026-09-20", "-9.96"),
			},
			entries: []firefly.Entry{
				s.eurEntry("423", "2026-09-17", "-9.97"),
				s.eurEntry("424", "2026-09-13", "-9.96"),
			},
			tolerance: defaultTolerance,
			missing:   []wantMissing{{label: "far-a"}, {label: "far-b"}},
		},
		{
			name: "a different amount, currency or sign never produces a hint",
			txs:  []bank.Transaction{s.booked("other", "2026-09-10", "-9.99")},
			entries: []firefly.Entry{
				s.eurEntry("431", "2026-09-12", "-9.98"),
				s.entry("432", "2026-09-12", "-9.99", "USD"),
				s.eurEntry("433", "2026-09-12", "9.99"),
			},
			tolerance: defaultTolerance,
			missing:   []wantMissing{{label: "other"}},
		},
		{
			name: "NearMiss may name an entry already paired with another transaction",
			txs: []bank.Transaction{
				s.booked("pn-a", "2026-09-10", "-1.11"),
				s.booked("pn-b", "2026-09-17", "-1.11"),
			},
			entries:   []firefly.Entry{s.eurEntry("741", "2026-09-12", "-1.11")},
			tolerance: defaultTolerance,
			matched:   []wantPair{{label: "pn-a", groupID: "741"}},
			missing: []wantMissing{
				{label: "pn-b", hint: s.hint(reconcile.NearMiss, "741", "2026-09-12")},
			},
		},
	}
}

// hintTieCases pin the FR-025a candidate order. Taken candidates lie within the tolerance and
// NearMiss ones strictly beyond it, so the two kinds can never be at the same distance; the last
// case pins the closest reachable contest, a Taken at the tolerance edge against a NearMiss one day
// beyond it that has the earlier date and the lower group id.
func (s *ReconcileSuite) hintTieCases() []reconcileCase {
	return []reconcileCase{
		{
			name: "the nearest candidate wins over an earlier date and a lower group id",
			txs:  []bank.Transaction{s.booked("near", "2026-09-10", "-2.22")},
			entries: []firefly.Entry{
				s.eurEntry("441", "2026-09-05", "-2.22"),
				s.eurEntry("449", "2026-09-14", "-2.22"),
			},
			tolerance: defaultTolerance,
			missing: []wantMissing{
				{label: "near", hint: s.hint(reconcile.NearMiss, "449", "2026-09-14")},
			},
		},
		{
			name: "at equal distance the earlier date wins over a lower group id",
			txs:  []bank.Transaction{s.booked("tie-date", "2026-09-10", "-2.23")},
			entries: []firefly.Entry{
				s.eurEntry("451", "2026-09-15", "-2.23"),
				s.eurEntry("459", "2026-09-05", "-2.23"),
			},
			tolerance: defaultTolerance,
			missing: []wantMissing{
				{label: "tie-date", hint: s.hint(reconcile.NearMiss, "459", "2026-09-05")},
			},
		},
		{
			name: "at equal distance and date the lower group id wins",
			txs:  []bank.Transaction{s.booked("tie-id", "2026-09-10", "-2.24")},
			entries: []firefly.Entry{
				s.eurEntry("462", "2026-09-15", "-2.24"),
				s.eurEntry("461", "2026-09-15", "-2.24"),
			},
			tolerance: defaultTolerance,
			missing: []wantMissing{
				{label: "tie-id", hint: s.hint(reconcile.NearMiss, "461", "2026-09-15")},
			},
		},
		{
			name: "at equal distance and date group ids compare numerically",
			txs:  []bank.Transaction{s.booked("tie-num", "2026-09-10", "-2.25")},
			entries: []firefly.Entry{
				s.eurEntry("100", "2026-09-15", "-2.25"),
				s.eurEntry("99", "2026-09-15", "-2.25"),
			},
			tolerance: defaultTolerance,
			missing: []wantMissing{
				{label: "tie-num", hint: s.hint(reconcile.NearMiss, "99", "2026-09-15")},
			},
		},
		{
			name: "two Taken candidates at equal distance: the earlier date wins",
			txs: []bank.Transaction{
				s.booked("tt-c", "2026-09-14", "-3.33"),
				s.booked("tt-b", "2026-09-13", "-3.33"),
				s.booked("tt-a", "2026-09-11", "-3.33"),
			},
			entries: []firefly.Entry{
				s.eurEntry("711", "2026-09-16", "-3.33"),
				s.eurEntry("712", "2026-09-12", "-3.33"),
			},
			tolerance: defaultTolerance,
			matched: []wantPair{
				{label: "tt-a", groupID: "712"},
				{label: "tt-b", groupID: "711"},
			},
			missing: []wantMissing{
				{label: "tt-c", hint: s.hint(reconcile.Taken, "712", "2026-09-12")},
			},
		},
		{
			name: "two Taken candidates on the same date: the lower group id wins",
			txs: []bank.Transaction{
				s.booked("ti-c", "2026-09-12", "-3.34"),
				s.booked("ti-b", "2026-09-10", "-3.34"),
				s.booked("ti-a", "2026-09-10", "-3.34"),
			},
			entries: []firefly.Entry{
				s.eurEntry("722", "2026-09-11", "-3.34"),
				s.eurEntry("721", "2026-09-11", "-3.34"),
			},
			tolerance: defaultTolerance,
			matched: []wantPair{
				{label: "ti-a", groupID: "721"},
				{label: "ti-b", groupID: "722"},
			},
			missing: []wantMissing{
				{label: "ti-c", hint: s.hint(reconcile.Taken, "721", "2026-09-11")},
			},
		},
		{
			name: "Taken at the tolerance edge beats NearMiss beyond it despite a later date and higher id",
			txs: []bank.Transaction{
				s.booked("tk-b", "2026-09-10", "-3.35"),
				s.booked("tk-a", "2026-09-10", "-3.35"),
			},
			entries: []firefly.Entry{
				s.eurEntry("739", "2026-09-13", "-3.35"),
				s.eurEntry("731", "2026-09-06", "-3.35"),
			},
			tolerance: defaultTolerance,
			matched:   []wantPair{{label: "tk-a", groupID: "739"}},
			missing: []wantMissing{
				{label: "tk-b", hint: s.hint(reconcile.Taken, "739", "2026-09-13")},
			},
		},
	}
}

// orderCases pin the output order contract documented on ReconcileSuite, for Missing and for
// Matched. The input is deliberately scrambled.
func (s *ReconcileSuite) orderCases() []reconcileCase {
	txs := []bank.Transaction{
		s.booked("o-1", "2026-09-12", "10.00"),
		withRef(s.booked("o-8", "2026-09-13", "-2.00"), ""),
		s.booked("o-2", "2026-09-12", "9.00"),
		s.tx("o-5", "2026-09-12", "-1.00", "USD", bank.Booked),
		s.booked("o-3", "2026-09-12", "-1.00"),
		withRef(s.booked("o-7", "2026-09-13", "-2.00"), ""),
		s.booked("o-4", "2026-09-11", "-50.00"),
		withRef(s.booked("o-6", "2026-09-12", "9.00"), "o-0"),
	}

	return []reconcileCase{
		{
			name:      "missing are ordered by date, currency, amount, EntryRef, then description",
			txs:       txs,
			tolerance: defaultTolerance,
			missing: []wantMissing{
				{label: "o-4"},
				{label: "o-3"},
				{label: "o-6"},
				{label: "o-2"},
				{label: "o-1"},
				{label: "o-5"},
				{label: "o-7"},
				{label: "o-8"},
			},
		},
		{
			name: "matched are ordered by the same key",
			txs:  txs,
			entries: []firefly.Entry{
				s.eurEntry("818", "2026-09-13", "-2.00"),
				s.eurEntry("811", "2026-09-12", "10.00"),
				s.eurEntry("813", "2026-09-12", "9.00"),
				s.entry("816", "2026-09-12", "-1.00", "USD"),
				s.eurEntry("814", "2026-09-12", "-1.00"),
				s.eurEntry("817", "2026-09-13", "-2.00"),
				s.eurEntry("815", "2026-09-11", "-50.00"),
				s.eurEntry("812", "2026-09-12", "9.00"),
			},
			tolerance: defaultTolerance,
			matched: []wantPair{
				{label: "o-4", groupID: "815"},
				{label: "o-3", groupID: "814"},
				{label: "o-6", groupID: "812"},
				{label: "o-2", groupID: "813"},
				{label: "o-1", groupID: "811"},
				{label: "o-5", groupID: "816"},
				{label: "o-7", groupID: "817"},
				{label: "o-8", groupID: "818"},
			},
		},
	}
}

// allCases is every table case of the suite, for TestDeterminism.
func (s *ReconcileSuite) allCases() []reconcileCase {
	return slices.Concat(
		s.scenarioCases(),
		s.toleranceCases(),
		s.greedyCases(),
		s.currencyAndSignCases(),
		s.windowCases(),
		s.voidCases(),
		s.dedupCases(),
		s.fireflyOnlyCases(),
		s.splitAndTransferCases(),
		s.hintCases(),
		s.hintTieCases(),
		s.orderCases(),
	)
}

// runCases runs each case as a subtest through assertCase.
func (s *ReconcileSuite) runCases(cases []reconcileCase) {
	for i := range cases {
		tc := &cases[i]
		s.Run(tc.name, func() { s.assertCase(tc, tc.txs, tc.entries) })
	}
}

// assertCase calls Reconcile with txs and entries (tc's input, possibly reordered) and asserts the
// FR-011 invariant and tc's complete expected result. It returns the result for further checks.
func (s *ReconcileSuite) assertCase(
	tc *reconcileCase,
	txs []bank.Transaction,
	entries []firefly.Entry,
) reconcile.Result {
	inWindow := s.inWindow(txs)
	s.Require().Equal(inWindow, len(tc.matched)+len(tc.missing)+tc.deduplicated+tc.void,
		"test data self-check: the expected result must satisfy the invariant too")

	txsBefore := slices.Clone(txs)
	entriesBefore := slices.Clone(entries)

	got := reconcile.Reconcile(txs, entries, tc.tolerance, s.window)

	s.Equal(txsBefore, txs, "Reconcile must not reorder or modify its bank input")
	s.Equal(entriesBefore, entries, "Reconcile must not reorder or modify its Firefly input")
	s.Equal(inWindow, len(got.Matched)+len(got.Missing)+got.Deduplicated+got.Void,
		"invariant: len(inWindow) == len(Matched)+len(Missing)+Deduplicated+Void")
	s.Equal(tc.matched, pairViews(got.Matched), "matched")
	s.Equal(tc.missing, missingViews(got.Missing), "missing")
	s.Equal(tc.deduplicated, got.Deduplicated, "deduplicated")
	s.Equal(tc.void, got.Void, "void")
	s.assertCarriedThrough(got, txs, entries)

	return got
}

// assertCarriedThrough checks that every Pair and Missing carries the caller's transaction and
// entry unchanged, not a rebuilt or partially copied value.
func (s *ReconcileSuite) assertCarriedThrough(got reconcile.Result, txs []bank.Transaction, entries []firefly.Entry) {
	txByLabel := make(map[txKey]bank.Transaction, len(txs))
	for i := range txs {
		txByLabel[keyOf(&txs[i])] = txs[i]
	}

	entryByID := make(map[string]firefly.Entry, len(entries))
	for _, e := range entries {
		entryByID[e.GroupID] = e
	}

	for i := range got.Matched {
		p := &got.Matched[i]
		s.Equal(txByLabel[keyOf(&p.Bank)], p.Bank, "matched bank transaction %s", p.Bank.Description)
		s.Equal(entryByID[p.Firefly.GroupID], p.Firefly, "matched Firefly entry %s", p.Firefly.GroupID)
	}

	for i := range got.Missing {
		m := &got.Missing[i]
		s.Equal(txByLabel[keyOf(&m.Tx)], m.Tx, "missing bank transaction %s", m.Tx.Description)
	}
}

// inWindow counts the bank transactions dated inside the window, before deduplication, computed
// from the test's own input and independently of Reconcile.
func (s *ReconcileSuite) inWindow(txs []bank.Transaction) int {
	n := 0

	for i := range txs {
		if d := txs[i].Date; !d.Before(s.window.From) && !d.After(s.window.To) {
			n++
		}
	}

	return n
}

// date parses a YYYY-MM-DD civil date.
func (s *ReconcileSuite) date(v string) civil.Date {
	d, err := civil.ParseDate(v)
	s.Require().NoError(err)

	return d
}

// amount parses a decimal amount in currency.
func (s *ReconcileSuite) amount(v, currency string) domain.Amount {
	a, err := domain.ParseAmount(v, currency)
	s.Require().NoError(err)

	return a
}

// tx builds a bank transaction on a fixed account. The label is both its Description and its
// EntryRef; withRef overrides the EntryRef.
func (s *ReconcileSuite) tx(label, date, amount, currency string, status bank.Status) bank.Transaction {
	return bank.Transaction{
		Account: bank.Account{
			BankKey:  "sample-bank",
			UID:      "00000000-0000-0000-0000-000000000000",
			Hash:     "hash-1",
			IBAN:     "LT000000000000000001",
			Currency: "EUR",
			Name:     "main",
		},
		Date:        s.date(date),
		Amount:      s.amount(amount, currency),
		Status:      status,
		EntryRef:    label,
		Description: label,
	}
}

// booked builds a booked EUR bank transaction.
func (s *ReconcileSuite) booked(label, date, amount string) bank.Transaction {
	return s.tx(label, date, amount, "EUR", bank.Booked)
}

// pending builds a pending EUR bank transaction.
func (s *ReconcileSuite) pending(label, date, amount string) bank.Transaction {
	return s.tx(label, date, amount, "EUR", bank.Pending)
}

// voided builds a void EUR bank transaction.
func (s *ReconcileSuite) voided(label, date, amount string) bank.Transaction {
	return s.tx(label, date, amount, "EUR", bank.Void)
}

// entry builds a Firefly entry on the mapped account.
func (s *ReconcileSuite) entry(groupID, date, amount, currency string) firefly.Entry {
	return firefly.Entry{
		GroupID:     groupID,
		AccountID:   "1",
		Date:        s.date(date),
		Amount:      s.amount(amount, currency),
		Description: "entry " + groupID,
	}
}

// eurEntry builds a EUR Firefly entry on the mapped account.
func (s *ReconcileSuite) eurEntry(groupID, date, amount string) firefly.Entry {
	return s.entry(groupID, date, amount, "EUR")
}

// hint builds an expected hint.
func (s *ReconcileSuite) hint(kind reconcile.HintKind, groupID, date string) *reconcile.Hint {
	return &reconcile.Hint{GroupID: groupID, Date: s.date(date), Kind: kind}
}

// txKey identifies a bank transaction within a case: its label and its status.
type txKey struct {
	label  string
	status bank.Status
}

// keyOf returns tx's txKey.
func keyOf(tx *bank.Transaction) txKey {
	return txKey{label: tx.Description, status: tx.Status}
}

// withRef returns tx with its EntryRef replaced.
func withRef(tx bank.Transaction, ref string) bank.Transaction {
	tx.EntryRef = ref

	return tx
}

// pairViews projects Matched to (label, group id) in output order, nil when empty.
func pairViews(pairs []reconcile.Pair) []wantPair {
	if len(pairs) == 0 {
		return nil
	}

	views := make([]wantPair, 0, len(pairs))
	for i := range pairs {
		views = append(views, wantPair{label: pairs[i].Bank.Description, groupID: pairs[i].Firefly.GroupID})
	}

	return views
}

// missingViews projects Missing to (label, flags, hint) in output order, nil when empty.
func missingViews(items []reconcile.Missing) []wantMissing {
	if len(items) == 0 {
		return nil
	}

	views := make([]wantMissing, 0, len(items))
	for i := range items {
		m := &items[i]
		views = append(views, wantMissing{
			label:        m.Tx.Description,
			pending:      m.Pending,
			lastReminder: m.LastReminder,
			hint:         m.Hint,
		})
	}

	return views
}

// stripHints returns views with every hint cleared, to compare results that may differ only in
// hints.
func stripHints(views []wantMissing) []wantMissing {
	out := slices.Clone(views)
	for i := range out {
		out[i].hint = nil
	}

	return out
}
