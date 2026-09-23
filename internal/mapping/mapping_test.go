package mapping_test

import (
	"github.com/stretchr/testify/suite"

	"github.com/Toshik1978/firefly-jar/internal/bank"
	"github.com/Toshik1978/firefly-jar/internal/firefly"
	"github.com/Toshik1978/firefly-jar/internal/mapping"
)

// AutoSuite covers automatic IBAN+currency resolution in Resolve (T037, FR-014, FR-016,
// data-model.md "Mapping" resolution order step 2, spec.md US3 scenarios 1-3). Every case here
// passes nil account rules, so only the auto-mapping branch of the resolution order is exercised;
// override and exclude rules are covered separately once T065/T066 add them.
type AutoSuite struct {
	suite.Suite
}

// newBankAccount builds a bank.Account distinguished by hash, with the given IBAN and currency, so
// each test case can construct accounts without repeating the fields Resolve ignores.
func newBankAccount(hash, iban, currency string) bank.Account {
	return bank.Account{
		BankKey:  "sample-bank",
		UID:      "uid-" + hash,
		Hash:     hash,
		IBAN:     iban,
		Currency: currency,
		Name:     "account " + hash,
	}
}

// newFireflyAccount builds a firefly.Account with the given id, IBAN, currency and active flag, so
// each test case can construct candidates without repeating the fields Resolve ignores.
func newFireflyAccount(id, iban, currency string, active bool) firefly.Account {
	return firefly.Account{
		ID:            id,
		Name:          "firefly " + id,
		IBAN:          iban,
		Currency:      currency,
		Role:          "defaultAsset",
		DecimalPlaces: 2,
		Active:        active,
	}
}

// candidateIDs extracts the Firefly account ids from a Mapping's Candidates, in the order Resolve
// returned them, so a test can assert the exact deterministic ordering by id. It returns nil rather
// than an empty slice for no candidates, so it compares equal to a literal nil expectation
// regardless of whether Resolve represents "no candidates" as a nil or a zero-length slice.
func candidateIDs(candidates []firefly.Account) []string {
	if len(candidates) == 0 {
		return nil
	}

	ids := make([]string, 0, len(candidates))
	for _, c := range candidates {
		ids = append(ids, c.ID)
	}

	return ids
}

// assertMapping checks Status, the Firefly pointer's target id (or nil) and the exact Candidates
// ids for a single Mapping, since the production Mapping struct also carries a Bank value and a
// Detail string that this suite does not constrain.
func (s *AutoSuite) assertMapping(
	got mapping.Mapping,
	wantStatus mapping.Status,
	wantFireflyID string,
	wantCandidateIDs []string,
) {
	s.Equal(wantStatus, got.Status, "status")

	if wantFireflyID == "" {
		s.Nil(got.Firefly, "firefly pointer")
	} else if s.NotNil(got.Firefly, "firefly pointer") {
		s.Equal(wantFireflyID, got.Firefly.ID, "firefly pointer target id")
	}

	s.Equal(wantCandidateIDs, candidateIDs(got.Candidates), "candidate ids")
}

// TestSingleBankAccount covers every case that resolves exactly one bank account against a set of
// Firefly III accounts and rules == nil.
func (s *AutoSuite) TestSingleBankAccount() {
	cases := []struct {
		name          string
		bankAccount   bank.Account
		fireflyAccts  []firefly.Account
		wantStatus    mapping.Status
		wantFireflyID string
		wantCandidate []string
	}{
		{
			name:          "exactly one active match by IBAN and currency is Auto",
			bankAccount:   newBankAccount("h1", "LT000000000000000001", "EUR"),
			fireflyAccts:  []firefly.Account{newFireflyAccount("10", "LT000000000000000001", "EUR", true)},
			wantStatus:    mapping.Auto,
			wantFireflyID: "10",
			wantCandidate: nil,
		},
		{
			name:        "two active matches is Ambiguous with both candidates ordered by id",
			bankAccount: newBankAccount("h2", "LT000000000000000002", "EUR"),
			fireflyAccts: []firefly.Account{
				// Deliberately out of id order: Resolve must sort Candidates by id, not by input order.
				newFireflyAccount("20", "LT000000000000000002", "EUR", true),
				newFireflyAccount("10", "LT000000000000000002", "EUR", true),
			},
			wantStatus:    mapping.Ambiguous,
			wantFireflyID: "",
			wantCandidate: []string{"10", "20"},
		},
		{
			name:          "no match is Unmapped",
			bankAccount:   newBankAccount("h3", "LT000000000000000003", "EUR"),
			fireflyAccts:  []firefly.Account{newFireflyAccount("30", "LT000000000000000099", "EUR", true)},
			wantStatus:    mapping.Unmapped,
			wantFireflyID: "",
			wantCandidate: nil,
		},
		{
			name:        "inactive account is never an auto candidate: the active twin still matches",
			bankAccount: newBankAccount("h4", "LT000000000000000004", "EUR"),
			fireflyAccts: []firefly.Account{
				newFireflyAccount("40", "LT000000000000000004", "EUR", false),
				newFireflyAccount("41", "LT000000000000000004", "EUR", true),
			},
			wantStatus:    mapping.Auto,
			wantFireflyID: "41",
			wantCandidate: nil,
		},
		{
			name:          "only an inactive match leaves the account Unmapped",
			bankAccount:   newBankAccount("h5", "LT000000000000000005", "EUR"),
			fireflyAccts:  []firefly.Account{newFireflyAccount("50", "LT000000000000000005", "EUR", false)},
			wantStatus:    mapping.Unmapped,
			wantFireflyID: "",
			wantCandidate: nil,
		},
		{
			// Carry-over from the T038 review: ids that parse as integers sort numerically, not
			// lexically, so id "10" is never ordered before id "9".
			name:        "candidate ids that parse as integers sort numerically, not lexically",
			bankAccount: newBankAccount("h10", "LT000000000000000010", "EUR"),
			fireflyAccts: []firefly.Account{
				newFireflyAccount("10", "LT000000000000000010", "EUR", true),
				newFireflyAccount("9", "LT000000000000000010", "EUR", true),
			},
			wantStatus:    mapping.Ambiguous,
			wantFireflyID: "",
			wantCandidate: []string{"9", "10"},
		},
		{
			name:          "a bank account without an IBAN is Unmapped even with a currency match",
			bankAccount:   newBankAccount("h6", "", "EUR"),
			fireflyAccts:  []firefly.Account{newFireflyAccount("60", "LT000000000000000006", "EUR", true)},
			wantStatus:    mapping.Unmapped,
			wantFireflyID: "",
			wantCandidate: nil,
		},
		{
			name:          "spaces and letter case in the IBAN are ignored on both sides",
			bankAccount:   newBankAccount("h7", "lt00 0000 0000 0000 0007", "EUR"),
			fireflyAccts:  []firefly.Account{newFireflyAccount("70", "LT000000000000000007", "EUR", true)},
			wantStatus:    mapping.Auto,
			wantFireflyID: "70",
			wantCandidate: nil,
		},
	}

	for i := range cases {
		tc := &cases[i]

		s.Run(tc.name, func() {
			got := mapping.Resolve([]bank.Account{tc.bankAccount}, tc.fireflyAccts, nil)

			s.Require().Len(got, 1, "one Mapping per bank account")
			s.Equal(tc.bankAccount, got[0].Bank, "bank account is carried through unchanged")
			s.assertMapping(got[0], tc.wantStatus, tc.wantFireflyID, tc.wantCandidate)
		})
	}
}

// TestMultiCurrencySameIBAN covers spec.md US3 scenario 2: a multi-currency bank exposes EUR and USD
// accounts under the same IBAN, and each must map to the Firefly III account of its own currency,
// not to the first IBAN match regardless of currency.
func (s *AutoSuite) TestMultiCurrencySameIBAN() {
	const iban = "LT000000000000000008"

	eurBank := newBankAccount("eur", iban, "EUR")
	usdBank := newBankAccount("usd", iban, "USD")

	eurFirefly := newFireflyAccount("80", iban, "EUR", true)
	usdFirefly := newFireflyAccount("81", iban, "USD", true)

	// Input order deliberately does not match currency order, so a correct Resolve cannot get this
	// right by accident of ordering.
	got := mapping.Resolve(
		[]bank.Account{eurBank, usdBank},
		[]firefly.Account{usdFirefly, eurFirefly},
		nil,
	)

	s.Require().Len(got, 2, "one Mapping per bank account, in input order")
	s.Equal(eurBank, got[0].Bank)
	s.assertMapping(got[0], mapping.Auto, "80", nil)
	s.Equal(usdBank, got[1].Bank)
	s.assertMapping(got[1], mapping.Auto, "81", nil)
}

// StatusSuite covers Status.String() for every declared value and for a value the type does not
// declare (T037 review minor, deferred to T038: contracts/cli.md fixes these exact lower-case
// words).
type StatusSuite struct {
	suite.Suite
}

// TestString checks every declared Status renders its contracts/cli.md word, and an undeclared
// value renders "unknown" rather than panicking or rendering empty.
func (s *StatusSuite) TestString() {
	cases := []struct {
		name   string
		status mapping.Status
		want   string
	}{
		{"auto", mapping.Auto, "auto"},
		{"override", mapping.Override, "override"},
		{"excluded", mapping.Excluded, "excluded"},
		{"ambiguous", mapping.Ambiguous, "ambiguous"},
		{"unmapped", mapping.Unmapped, "unmapped"},
		{"unknown value", mapping.Status(99), "unknown"},
	}

	for _, tc := range cases {
		s.Run(tc.name, func() {
			s.Equal(tc.want, tc.status.String())
		})
	}
}
