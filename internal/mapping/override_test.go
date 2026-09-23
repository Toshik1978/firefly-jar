package mapping_test

import (
	"github.com/stretchr/testify/suite"

	"github.com/Toshik1978/firefly-jar/internal/bank"
	"github.com/Toshik1978/firefly-jar/internal/config"
	"github.com/Toshik1978/firefly-jar/internal/firefly"
	"github.com/Toshik1978/firefly-jar/internal/mapping"
)

// OverrideSuite covers the override/exclude step of the resolution order in Resolve (T065/T066,
// FR-015, data-model.md "Mapping" resolution order step 1, spec.md US3). Every Firefly III account
// used as an override target in this suite deliberately has an IBAN and currency that do not match
// the bank account it targets, so a case only passes when Resolve honors the rule itself rather than
// falling back to the automatic IBAN-and-currency step.
type OverrideSuite struct {
	suite.Suite
}

// wantOverride is one Mapping's expected Status, Firefly pointer target id (empty for none) and
// exact Detail.
type wantOverride struct {
	status    mapping.Status
	fireflyID string
	detail    string
}

// assertMapping checks Status, the Firefly pointer's target id (or nil) and the exact Detail for a
// single Mapping.
func (s *OverrideSuite) assertMapping(got mapping.Mapping, want wantOverride) {
	s.Equal(want.status, got.Status, "status")

	if want.fireflyID == "" {
		s.Nil(got.Firefly, "firefly pointer")
	} else if s.NotNil(got.Firefly, "firefly pointer") {
		s.Equal(want.fireflyID, got.Firefly.ID, "firefly pointer target id")
	}

	s.Equal(want.detail, got.Detail, "detail")
}

// TestOverrideAndExclude covers every accounts: rule case from data-model.md "Mapping" resolution
// order step 1: hash and iban rules (with and without currency), exclude, precedence over
// auto-mapping, first-rule-wins, bank-key isolation, a missing override target, and an override
// targeting an inactive account.
func (s *OverrideSuite) TestOverrideAndExclude() {
	cases := []struct {
		name         string
		bankAccts    []bank.Account
		fireflyAccts []firefly.Account
		rules        []config.AccountRule
		want         []wantOverride
	}{
		{
			name:      "hash rule maps to the override target",
			bankAccts: []bank.Account{newBankAccount("h1", "LT000000000000000101", "EUR")},
			fireflyAccts: []firefly.Account{
				newFireflyAccount("100", "LT000000000000000199", "USD", true),
			},
			rules: []config.AccountRule{
				{Bank: "sample-bank", Hash: "h1", FireflyAccountID: "100"},
			},
			want: []wantOverride{
				{status: mapping.Override, fireflyID: "100"},
			},
		},
		{
			name: "iban rule without currency matches every currency of that iban",
			bankAccts: []bank.Account{
				newBankAccount("h2eur", "LT000000000000000102", "EUR"),
				newBankAccount("h2usd", "LT000000000000000102", "USD"),
			},
			fireflyAccts: []firefly.Account{
				newFireflyAccount("110", "LT000000000000000199", "GBP", true),
			},
			rules: []config.AccountRule{
				{Bank: "sample-bank", IBAN: "LT000000000000000102", FireflyAccountID: "110"},
			},
			want: []wantOverride{
				{status: mapping.Override, fireflyID: "110"},
				{status: mapping.Override, fireflyID: "110"},
			},
		},
		{
			name: "iban rule with currency matches only that currency; the other falls through to auto",
			bankAccts: []bank.Account{
				newBankAccount("h3eur", "LT000000000000000103", "EUR"),
				newBankAccount("h3usd", "LT000000000000000103", "USD"),
			},
			fireflyAccts: []firefly.Account{
				newFireflyAccount("120", "LT000000000000000198", "GBP", true),
				newFireflyAccount("121", "LT000000000000000103", "EUR", true),
			},
			rules: []config.AccountRule{
				{Bank: "sample-bank", IBAN: "LT000000000000000103", Currency: "USD", FireflyAccountID: "120"},
			},
			want: []wantOverride{
				{status: mapping.Auto, fireflyID: "121"},
				{status: mapping.Override, fireflyID: "120"},
			},
		},
		{
			name:      "exclude true maps to Excluded with a nil Firefly pointer",
			bankAccts: []bank.Account{newBankAccount("h4", "LT000000000000000104", "EUR")},
			rules: []config.AccountRule{
				{Bank: "sample-bank", Hash: "h4", Exclude: true},
			},
			want: []wantOverride{
				{status: mapping.Excluded, fireflyID: ""},
			},
		},
		{
			name:      "an override takes precedence over an account that would otherwise auto-map",
			bankAccts: []bank.Account{newBankAccount("h5", "LT000000000000000105", "EUR")},
			fireflyAccts: []firefly.Account{
				// Would be the unique Auto match if the override rule were ignored.
				newFireflyAccount("130", "LT000000000000000105", "EUR", true),
				newFireflyAccount("131", "LT000000000000000197", "GBP", true),
			},
			rules: []config.AccountRule{
				{Bank: "sample-bank", Hash: "h5", FireflyAccountID: "131"},
			},
			want: []wantOverride{
				{status: mapping.Override, fireflyID: "131"},
			},
		},
		{
			name:      "the first matching rule wins over a later rule for the same account",
			bankAccts: []bank.Account{newBankAccount("h6", "LT000000000000000106", "EUR")},
			fireflyAccts: []firefly.Account{
				newFireflyAccount("140", "LT000000000000000196", "GBP", true),
				newFireflyAccount("141", "LT000000000000000195", "GBP", true),
			},
			rules: []config.AccountRule{
				{Bank: "sample-bank", Hash: "h6", FireflyAccountID: "140"},
				{Bank: "sample-bank", Hash: "h6", FireflyAccountID: "141"},
			},
			want: []wantOverride{
				{status: mapping.Override, fireflyID: "140"},
			},
		},
		{
			name:      "a rule for another bank key never matches",
			bankAccts: []bank.Account{newBankAccount("h7", "LT000000000000000107", "EUR")},
			fireflyAccts: []firefly.Account{
				newFireflyAccount("150", "LT000000000000000194", "GBP", true),
			},
			rules: []config.AccountRule{
				{Bank: "other-bank", Hash: "h7", FireflyAccountID: "150"},
			},
			want: []wantOverride{
				{status: mapping.Unmapped, fireflyID: ""},
			},
		},
		{
			name:      "an override to a non-existent Firefly id is Unmapped with an exact detail",
			bankAccts: []bank.Account{newBankAccount("h8", "LT000000000000000108", "EUR")},
			rules: []config.AccountRule{
				{Bank: "sample-bank", Hash: "h8", FireflyAccountID: "999"},
			},
			want: []wantOverride{
				{status: mapping.Unmapped, fireflyID: "", detail: "override target #999 not found"},
			},
		},
		{
			name:      "an override may target an inactive Firefly account",
			bankAccts: []bank.Account{newBankAccount("h9", "LT000000000000000109", "EUR")},
			fireflyAccts: []firefly.Account{
				newFireflyAccount("160", "LT000000000000000193", "GBP", false),
			},
			rules: []config.AccountRule{
				{Bank: "sample-bank", Hash: "h9", FireflyAccountID: "160"},
			},
			want: []wantOverride{
				{status: mapping.Override, fireflyID: "160"},
			},
		},
	}

	for i := range cases {
		tc := &cases[i]

		s.Run(tc.name, func() {
			got := mapping.Resolve(tc.bankAccts, tc.fireflyAccts, tc.rules)

			s.Require().Len(got, len(tc.want), "one Mapping per bank account")

			for j, want := range tc.want {
				s.Equal(tc.bankAccts[j], got[j].Bank, "bank account is carried through unchanged")
				s.assertMapping(got[j], want)
			}
		})
	}
}
