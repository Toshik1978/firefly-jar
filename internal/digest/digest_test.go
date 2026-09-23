// Package digest_test pins the plain-text reminder digest (T043, FR-025, FR-025a, FR-036,
// contracts/digest.md): the header, the section order, the account headings and transaction lines,
// their sorting, description sanitizing, and that no full IBAN ever reaches the text. It is the RED
// half of T044. Three golden files under testdata/digest pin whole digests byte for byte; they are
// regenerated only with UPDATE_GOLDEN=1 and are reviewed like code.
package digest_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/suite"

	"github.com/Toshik1978/firefly-jar/internal/accountmap"
	"github.com/Toshik1978/firefly-jar/internal/bank"
	"github.com/Toshik1978/firefly-jar/internal/civil"
	"github.com/Toshik1978/firefly-jar/internal/config"
	"github.com/Toshik1978/firefly-jar/internal/digest"
	"github.com/Toshik1978/firefly-jar/internal/firefly"
	"github.com/Toshik1978/firefly-jar/internal/money"
	"github.com/Toshik1978/firefly-jar/internal/reconcile"
	"github.com/Toshik1978/firefly-jar/internal/report"
)

// TestDigest is the single entry point for package digest's test suites.
func TestDigest(t *testing.T) {
	suite.Run(t, new(RenderSuite))
}

// goldenDir holds the reviewed golden digests, relative to this package directory.
const goldenDir = "../../testdata/digest"

// Anonymized, IBAN-shaped fixture identifiers. Every one of them must come out masked.
const (
	ibanMain    = "LT120000000000003456"
	ibanSavings = "LT120000000000007890"
	ibanRevolut = "LT990000000000000001"
	ibanLow     = "LT110000000000000002"
	ibanInText  = "LT770000000000000042"
	// ibanSpaced is ibanInText in its print form, 4-character groups separated by spaces.
	ibanSpaced = "LT77 0000 0000 0000 0042"
	hashWise   = "ab12cd34ef56ab78cd90ef12"
)

// headerWindow ends the header line of every digest rendered over the default fixture window.
const headerWindow = "(window 2026-08-24 – 2026-09-22)"

// RenderSuite covers digest.Render, Digest.Text and Digest.Subject (T043).
type RenderSuite struct {
	suite.Suite
}

// goldenCase is one whole-digest golden comparison.
type goldenCase struct {
	name   string
	report report.RunReport
}

// lineCase is one rendered transaction line: a single missing item on an account whose Firefly III
// account has the given precision (nil Firefly when unmapped is set).
type lineCase struct {
	name     string
	missing  reconcile.Missing
	places   uint8
	unmapped bool
	want     string
}

// descriptionCase is one raw bank description and the text the digest line must show for it.
type descriptionCase struct {
	name string
	raw  string
	want string
}

// headingCase is one account heading rendered for a bank account under a banks: map.
type headingCase struct {
	name string
	acct bank.Account
	want string
}

// TestGolden renders three fixture reports and compares Text byte for byte with the reviewed golden
// files. It also checks the shape every digest shares: Subject is the header, the first line;
// Text is Lines joined by newlines with a trailing newline; no line contains a newline of its own,
// so the Telegram notifier can split on Lines safely.
func (s *RenderSuite) TestGolden() {
	cases := []goldenCase{
		{name: "full", report: s.fullReport()},
		{name: "missing_only", report: s.missingOnlyReport()},
		{name: "consent_only", report: s.consentOnlyReport()},
	}

	for i := range cases {
		tc := &cases[i]
		s.Run(tc.name, func() {
			d := digest.Render(tc.report, banks())

			s.Require().NotEmpty(d.Lines)
			s.Equal(d.Lines[0], d.Subject, "subject is the header line")
			s.Equal(strings.Join(d.Lines, "\n")+"\n", d.Text(), "text joins lines")

			for _, line := range d.Lines {
				s.NotContains(line, "\n", "a line never spans two lines")
			}

			s.assertGolden(tc.name, d.Text())
		})
	}
}

// TestHeader pins the header line: the missing count, the unchecked-account count with singular
// "account" only for exactly one, and the inclusive window. Excluded accounts count toward
// neither, and a checked account with nothing missing adds nothing.
func (s *RenderSuite) TestHeader() {
	one := uncheckedAccount(bankAccount("swedbank", ibanSavings, "", "EUR", "Savings"), report.RateLimited, "")
	two := uncheckedAccount(bankAccount("revolut", ibanRevolut, "", "USD", "Revolut USD"), report.BankError, "")
	missing := s.checkedAccount(bankAccount("swedbank", ibanMain, "", "EUR", "Main"), 2,
		s.missing("2026-09-20", "-4.50", "EUR", "COFFEE SHOP"))
	excluded := s.excludedAccount(bankAccount("seb", ibanLow, "", "EUR", "Old"),
		s.missing("2026-09-20", "-1.00", "EUR", "IGNORED"))
	clean := s.checkedAccount(bankAccount("revolut", ibanRevolut, "", "EUR", "Revolut EUR"), 2)

	cases := []struct {
		name     string
		accounts []report.AccountResult
		want     string
	}{
		{"nothing", nil, "firefly-jar: 0 missing, 0 unchecked accounts " + headerWindow},
		{"one unchecked is singular", []report.AccountResult{one}, "firefly-jar: 0 missing, 1 unchecked account " +
			headerWindow},
		{"two unchecked is plural", []report.AccountResult{one, two}, "firefly-jar: 0 missing, 2 unchecked accounts " +
			headerWindow},
		{"one missing", []report.AccountResult{missing}, "firefly-jar: 1 missing, 0 unchecked accounts " +
			headerWindow},
		{
			"excluded and clean accounts count for nothing",
			[]report.AccountResult{excluded, clean, missing, one},
			"firefly-jar: 1 missing, 1 unchecked account " + headerWindow,
		},
	}

	for _, tc := range cases {
		s.Run(tc.name, func() {
			d := digest.Render(report.RunReport{Window: s.window(), Accounts: tc.accounts}, banks())

			s.Require().NotEmpty(d.Lines)
			s.Equal(tc.want, d.Lines[0])
			s.Equal(tc.want, d.Subject, "subject equals the header line")
		})
	}
}

// TestEmptyReportIsHeaderOnly pins that every section is omitted when it has nothing to show, down
// to a report with nothing at all.
func (s *RenderSuite) TestEmptyReportIsHeaderOnly() {
	d := digest.Render(report.RunReport{Window: s.window()}, banks())

	s.Equal([]string{"firefly-jar: 0 missing, 0 unchecked accounts " + headerWindow}, d.Lines)
	s.Equal("firefly-jar: 0 missing, 0 unchecked accounts "+headerWindow+"\n", d.Text())
}

// TestSectionOrderAndOmission pins the order Problems, Consent, Missing in Firefly III, a blank
// line after the header and between sections, and omission of every empty section.
func (s *RenderSuite) TestSectionOrderAndOmission() {
	problem := report.Problem{Scope: "firefly", Reason: "unreachable"}
	warning := report.ConsentWarning{BankKey: "seb", ValidUntil: validUntil(2026, 9, 27), DaysLeft: 5}
	missing := s.checkedAccount(bankAccount("swedbank", ibanMain, "", "EUR", "Main"), 2,
		s.missing("2026-09-20", "-4.50", "EUR", "COFFEE SHOP"))

	const (
		problemLine = "- firefly: unreachable"
		consentLine = "- seb: consent expires 2026-09-27 (5 days) — run: firefly-jar auth seb"
		heading     = "Swedbank · LT12…3456 · Main (EUR)"
		missingLine = "- 2026-09-20  -4.50 EUR  COFFEE SHOP"
	)

	cases := []struct {
		name string
		rep  report.RunReport
		want []string
	}{
		{
			name: "problems only",
			rep:  report.RunReport{Problems: []report.Problem{problem}},
			want: []string{"", "⚠ Problems", problemLine},
		},
		{
			name: "consent only",
			rep:  report.RunReport{ConsentWarnings: []report.ConsentWarning{warning}},
			want: []string{"", "⏰ Consent", consentLine},
		},
		{
			name: "missing only",
			rep:  report.RunReport{Accounts: []report.AccountResult{missing}},
			want: []string{"", "Missing in Firefly III", heading, missingLine},
		},
		{
			name: "problems and missing, no consent",
			rep:  report.RunReport{Problems: []report.Problem{problem}, Accounts: []report.AccountResult{missing}},
			want: []string{"", "⚠ Problems", problemLine, "", "Missing in Firefly III", heading, missingLine},
		},
		{
			name: "all three in order",
			rep: report.RunReport{
				Accounts:        []report.AccountResult{missing},
				ConsentWarnings: []report.ConsentWarning{warning},
				Problems:        []report.Problem{problem},
			},
			want: []string{
				"", "⚠ Problems", problemLine,
				"", "⏰ Consent", consentLine,
				"", "Missing in Firefly III", heading, missingLine,
			},
		},
	}

	for i := range cases {
		tc := &cases[i]
		s.Run(tc.name, func() {
			tc.rep.Window = s.window()
			d := digest.Render(tc.rep, banks())

			s.Require().NotEmpty(d.Lines)
			s.Equal(tc.want, d.Lines[1:])
		})
	}
}

// TestAccountHeading pins `<bank display> · <masked IBAN or hash:xxxx…xxxx> · <name> (<CUR>)`: the
// bank's display name when configured, else its name, else (a bank key missing from banks:) the key
// itself; the masked IBAN, else the masked hash.
func (s *RenderSuite) TestAccountHeading() {
	cases := []headingCase{
		{
			name: "display name wins over name",
			acct: bankAccount("swedbank", ibanMain, hashWise, "EUR", "Main"),
			want: "Swedbank · LT12…3456 · Main (EUR)",
		},
		{
			name: "name when display is empty",
			acct: bankAccount("revolut", ibanRevolut, "", "USD", "Revolut USD"),
			want: "Revolut · LT99…0001 · Revolut USD (USD)",
		},
		{
			name: "hash when the account has no IBAN",
			acct: bankAccount("wise", "", hashWise, "JPY", "Yen"),
			want: "Wise · hash:ab12…ef12 · Yen (JPY)",
		},
		{
			name: "bank key when banks has no entry for it",
			acct: bankAccount("unknownbank", ibanLow, "", "EUR", "Spare"),
			want: "unknownbank · LT11…0002 · Spare (EUR)",
		},
	}

	for i := range cases {
		tc := &cases[i]
		s.Run(tc.name, func() {
			acct := s.checkedAccount(tc.acct, 2, s.missing("2026-09-20", "-4.50", tc.acct.Currency, "COFFEE SHOP"))
			d := digest.Render(report.RunReport{Window: s.window(), Accounts: []report.AccountResult{acct}}, banks())

			s.Require().Len(d.Lines, 5)
			s.Equal("Missing in Firefly III", d.Lines[2])
			s.Equal(tc.want, d.Lines[3])
		})
	}
}

// TestLineFormat pins `- <date>  <signed amount> <CUR>  <description>` with two spaces between
// fields, the flags `⏳ pending` then `🔚 last reminder`, then the hint, each after two spaces.
// Amounts use the mapped Firefly III account's precision (2 when no Firefly III account is set),
// and a credit carries no plus sign.
func (s *RenderSuite) TestLineFormat() {
	both := s.missing("2026-08-24", "-4.50", "EUR", "COFFEE SHOP")
	both.Pending = true
	both.LastReminder = true
	both.Hint = &reconcile.Hint{GroupID: "7", Date: s.date("2026-08-25"), Kind: reconcile.Taken}

	pending := s.missing("2026-09-21", "-4.50", "EUR", "COFFEE SHOP")
	pending.Pending = true

	last := s.missing("2026-08-24", "-12.00", "EUR", "STREAMING SERVICE")
	last.LastReminder = true

	cases := []lineCase{
		{
			name:    "plain debit",
			missing: s.missing("2026-09-19", "-63.12", "EUR", "SUPERMARKET"),
			places:  2,
			want:    "- 2026-09-19  -63.12 EUR  SUPERMARKET",
		},
		{
			name:    "credit has no plus sign",
			missing: s.missing("2026-09-19", "150", "EUR", "REFUND"),
			places:  2,
			want:    "- 2026-09-19  150.00 EUR  REFUND",
		},
		{name: "pending flag", missing: pending, places: 2, want: "- 2026-09-21  -4.50 EUR  COFFEE SHOP  ⏳ pending"},
		{
			name:    "last reminder flag",
			missing: last,
			places:  2,
			want:    "- 2026-08-24  -12.00 EUR  STREAMING SERVICE  🔚 last reminder",
		},
		{
			name:    "both flags then the hint",
			missing: both,
			places:  2,
			want: "- 2026-08-24  -4.50 EUR  COFFEE SHOP  ⏳ pending  🔚 last reminder  " +
				"≈ Firefly #7 on 2026-08-25 (paired with another)",
		},
		{
			name:    "zero decimal places",
			missing: s.missing("2026-09-05", "-1200", "JPY", "RAMEN BAR"),
			places:  0,
			want:    "- 2026-09-05  -1200 JPY  RAMEN BAR",
		},
		{
			name:    "three decimal places",
			missing: s.missing("2026-09-05", "-1.25", "BHD", "TAXI"),
			places:  3,
			want:    "- 2026-09-05  -1.250 BHD  TAXI",
		},
		{
			name:     "no Firefly III account defaults to two places",
			missing:  s.missing("2026-09-05", "-4.5", "EUR", "COFFEE SHOP"),
			unmapped: true,
			want:     "- 2026-09-05  -4.50 EUR  COFFEE SHOP",
		},
	}

	for i := range cases {
		tc := &cases[i]
		s.Run(tc.name, func() {
			s.Equal(tc.want, s.renderLine(tc))
		})
	}
}

// TestHints pins both hint kinds (FR-025a): Taken reads "(paired with another)" and NearMiss reads
// "(N days apart)" with N the absolute day distance, whichever side of the bank date the entry is.
func (s *RenderSuite) TestHints() {
	withHint := func(txDate, hintDate, group string, kind reconcile.HintKind) reconcile.Missing {
		m := s.missing(txDate, "-25.00", "EUR", "CITY PARKING")
		m.Hint = &reconcile.Hint{GroupID: group, Date: s.date(hintDate), Kind: kind}

		return m
	}

	cases := []lineCase{
		{
			name:    "taken",
			missing: withHint("2026-09-15", "2026-09-13", "7", reconcile.Taken),
			places:  2,
			want:    "- 2026-09-15  -25.00 EUR  CITY PARKING  ≈ Firefly #7 on 2026-09-13 (paired with another)",
		},
		{
			name:    "near miss after the bank date",
			missing: withHint("2026-09-10", "2026-09-15", "9", reconcile.NearMiss),
			places:  2,
			want:    "- 2026-09-10  -25.00 EUR  CITY PARKING  ≈ Firefly #9 on 2026-09-15 (5 days apart)",
		},
		{
			name:    "near miss before the bank date",
			missing: withHint("2026-09-15", "2026-09-12", "11", reconcile.NearMiss),
			places:  2,
			want:    "- 2026-09-15  -25.00 EUR  CITY PARKING  ≈ Firefly #11 on 2026-09-12 (3 days apart)",
		},
		{
			name:    "near miss across a month end",
			missing: withHint("2026-09-02", "2026-08-29", "12", reconcile.NearMiss),
			places:  2,
			want:    "- 2026-09-02  -25.00 EUR  CITY PARKING  ≈ Firefly #12 on 2026-08-29 (4 days apart)",
		},
	}

	for i := range cases {
		tc := &cases[i]
		s.Run(tc.name, func() {
			s.Equal(tc.want, s.renderLine(tc))
		})
	}
}

// TestLineSorting pins the order of lines within one account: date descending, then amount by
// signed numeric value ascending (not as text), then description.
func (s *RenderSuite) TestLineSorting() {
	acct := s.checkedAccount(bankAccount("swedbank", ibanMain, "", "EUR", "Main"), 2,
		s.missing("2026-09-10", "-4.50", "EUR", "BAKERY"),
		s.missing("2026-09-20", "20.00", "EUR", "REFUND"),
		s.missing("2026-09-20", "-4.50", "EUR", "COFFEE SHOP"),
		s.missing("2026-09-20", "9.00", "EUR", "CASHBACK"),
		s.missing("2026-09-15", "-100.00", "EUR", "RENT"),
		s.missing("2026-09-20", "-63.12", "EUR", "SUPERMARKET"),
		s.missing("2026-09-20", "-4.50", "EUR", "BAKERY"),
	)

	d := digest.Render(report.RunReport{Window: s.window(), Accounts: []report.AccountResult{acct}}, banks())

	s.Require().Len(d.Lines, 11)
	s.Equal([]string{
		"- 2026-09-20  -63.12 EUR  SUPERMARKET",
		"- 2026-09-20  -4.50 EUR  BAKERY",
		"- 2026-09-20  -4.50 EUR  COFFEE SHOP",
		"- 2026-09-20  9.00 EUR  CASHBACK",
		"- 2026-09-20  20.00 EUR  REFUND",
		"- 2026-09-15  -100.00 EUR  RENT",
		"- 2026-09-10  -4.50 EUR  BAKERY",
	}, d.Lines[4:])
}

// TestAccountSorting pins the order of account blocks: bank key, then masked identifier (as text,
// so a masked IBAN sorts before a "hash:" one), then currency. Accounts with nothing missing get no
// block at all.
func (s *RenderSuite) TestAccountSorting() {
	line := func() reconcile.Missing { return s.missing("2026-09-20", "-1.00", "EUR", "FEE") }

	accounts := []report.AccountResult{
		s.checkedAccount(bankAccount("swedbank", ibanMain, "", "EUR", "Main"), 2, line()),
		s.checkedAccount(bankAccount("revolut", "", hashWise, "EUR", "Hashed"), 2, line()),
		s.checkedAccount(bankAccount("revolut", ibanRevolut, "", "USD", "Revolut USD"), 2, line()),
		s.checkedAccount(bankAccount("revolut", ibanLow, "", "EUR", "Low"), 2, line()),
		s.checkedAccount(bankAccount("revolut", ibanRevolut, "", "GBP", "Clean"), 2),
		s.checkedAccount(bankAccount("revolut", ibanRevolut, "", "EUR", "Revolut EUR"), 2, line()),
	}

	d := digest.Render(report.RunReport{Window: s.window(), Accounts: accounts}, banks())

	s.Equal([]string{
		"Missing in Firefly III",
		"Revolut · LT11…0002 · Low (EUR)",
		"- 2026-09-20  -1.00 EUR  FEE",
		"Revolut · LT99…0001 · Revolut EUR (EUR)",
		"- 2026-09-20  -1.00 EUR  FEE",
		"Revolut · LT99…0001 · Revolut USD (USD)",
		"- 2026-09-20  -1.00 EUR  FEE",
		"Revolut · hash:ab12…ef12 · Hashed (EUR)",
		"- 2026-09-20  -1.00 EUR  FEE",
		"Swedbank · LT12…3456 · Main (EUR)",
		"- 2026-09-20  -1.00 EUR  FEE",
	}, d.Lines[2:])
}

// TestExcludedAccountIsNeverRendered pins that an excluded account shows nowhere, even when a
// leftover Result carries missing transactions.
func (s *RenderSuite) TestExcludedAccountIsNeverRendered() {
	excluded := s.excludedAccount(bankAccount("seb", ibanLow, "", "EUR", "Old"),
		s.missing("2026-09-20", "-1.00", "EUR", "IGNORED"))

	d := digest.Render(report.RunReport{Window: s.window(), Accounts: []report.AccountResult{excluded}}, banks())

	s.Equal([]string{"firefly-jar: 0 missing, 0 unchecked accounts " + headerWindow}, d.Lines)
}

// TestDescription pins description sanitizing: control characters (Unicode Cc), format characters
// (Cf, which includes every bidi override and isolate) and the line and paragraph separators are
// removed so a counterparty name can neither break a line nor spoof one; IBAN-shaped text is
// masked (FR-036); the result is cut to 60 characters (runes) including a trailing "…"; and an
// empty result reads "(no description)". Sanitizing happens before the length is measured.
func (s *RenderSuite) TestDescription() {
	sixty := strings.Repeat("A", 60)

	cases := []descriptionCase{
		{name: "short text is kept", raw: "COFFEE SHOP", want: "COFFEE SHOP"},
		{name: "exactly sixty characters is kept", raw: sixty, want: sixty},
		{name: "sixty-one characters is cut to sixty", raw: sixty + "B", want: strings.Repeat("A", 59) + "…"},
		{
			name: "length counts characters, not bytes",
			raw:  strings.Repeat("Ž", 70),
			want: strings.Repeat("Ž", 59) + "…",
		},
		{
			name: "control characters are removed",
			raw:  "A\x00B\x07C\x1bD\x7fE\u0085F\tG\nH\rI",
			want: "ABCDEFGHI",
		},
		{
			name: "bidi overrides and isolates are removed",
			raw:  "EVIL\u202eTXT.EXE \u2066A\u2069 \u202aB\u202c \u200eC\u200f",
			want: "EVILTXT.EXE A B C",
		},
		{
			name: "other format characters are removed",
			raw:  "A\u200bB\u200dC\ufeffD\u00adE\u2060F",
			want: "ABCDEF",
		},
		{name: "line and paragraph separators are removed", raw: "A\u2028B\u2029C", want: "ABC"},
		{name: "removed characters do not count toward the limit", raw: sixty + "\u202e\x00", want: sixty},
		{name: "empty reads no description", raw: "", want: "(no description)"},
		{name: "nothing left after sanitizing reads no description", raw: "\u202e\x00\u2028", want: "(no description)"},
		{name: "an IBAN in the text is masked", raw: "TRANSFER TO " + ibanInText, want: "TRANSFER TO LT77…0042"},
		{
			name: "a spaced IBAN in the text is masked",
			raw:  "TRANSFER TO " + ibanSpaced + " REF 7",
			want: "TRANSFER TO LT77…0042 REF 7",
		},
		{
			name: "stripping comes before masking, so a hidden character cannot shield an IBAN",
			raw:  "TO LT77\u200b0000000000000042",
			want: "TO LT77…0042",
		},
		{
			name: "masking comes before truncating, so a masked IBAN that fits is kept whole",
			raw:  strings.Repeat("A", 50) + " " + ibanInText,
			want: strings.Repeat("A", 50) + " LT77…0042",
		},
	}

	for i := range cases {
		tc := &cases[i]
		s.Run(tc.name, func() {
			line := s.renderLine(&lineCase{missing: s.missing("2026-09-20", "-4.50", "EUR", tc.raw), places: 2})

			s.Equal("- 2026-09-20  -4.50 EUR  "+tc.want, line)
		})
	}
}

// TestConsentLines pins `- <bank key>: consent expires YYYY-MM-DD (N days) — run: firefly-jar auth
// <bank key>`, always the config key (the name `auth` takes, never the display name), sorted by
// bank key, with "(1 day)" singular and "(0 days)" plural. The date is ValidUntil's calendar date in
// its own Location: converting to the configured zone is the caller's job.
func (s *RenderSuite) TestConsentLines() {
	lateUTC := time.Date(2026, 9, 27, 23, 30, 0, 0, time.UTC).In(time.FixedZone("x", 3*3600))

	rep := report.RunReport{
		Window: s.window(),
		ConsentWarnings: []report.ConsentWarning{
			{BankKey: "swedbank", ValidUntil: validUntil(2026, 10, 1), DaysLeft: 9},
			{BankKey: "seb", ValidUntil: validUntil(2026, 9, 27), DaysLeft: 5},
			{BankKey: "wise", ValidUntil: validUntil(2026, 9, 22), DaysLeft: 0},
			{BankKey: "revolut", ValidUntil: validUntil(2026, 9, 23), DaysLeft: 1},
			{BankKey: "luminor", ValidUntil: lateUTC, DaysLeft: 6},
		},
	}

	d := digest.Render(rep, banks())

	s.Equal([]string{
		"",
		"⏰ Consent",
		"- luminor: consent expires 2026-09-28 (6 days) — run: firefly-jar auth luminor",
		"- revolut: consent expires 2026-09-23 (1 day) — run: firefly-jar auth revolut",
		"- seb: consent expires 2026-09-27 (5 days) — run: firefly-jar auth seb",
		"- swedbank: consent expires 2026-10-01 (9 days) — run: firefly-jar auth swedbank",
		"- wise: consent expires 2026-09-22 (0 days) — run: firefly-jar auth wise",
	}, d.Lines[1:])
}

// TestUncheckedDetail pins how Unchecked.Detail is shown: appended as `: <detail>` after the reason
// text when non-empty, through the same pipeline as descriptions (strip control, format and
// separator characters, then mask IBANs, then cut to 60 characters with "…"), and left off entirely
// when nothing is left after stripping.
func (s *RenderSuite) TestUncheckedDetail() {
	const prefix = "- swedbank LT12…7890 (Savings): unchecked — bank error"

	cases := []descriptionCase{
		{name: "empty detail adds nothing", raw: "", want: prefix},
		{name: "plain detail", raw: "HTTP 503 from provider", want: prefix + ": HTTP 503 from provider"},
		{name: "control and bidi characters are stripped", raw: "HTTP\n503\u202e", want: prefix + ": HTTP503"},
		{name: "an IBAN is masked", raw: "account " + ibanInText, want: prefix + ": account LT77…0042"},
		{
			name: "a long detail is cut to sixty characters",
			raw:  strings.Repeat("E", 61),
			want: prefix + ": " + strings.Repeat("E", 59) + "…",
		},
		{name: "nothing left after stripping adds nothing", raw: "\u202e\x00", want: prefix},
	}

	for i := range cases {
		tc := &cases[i]
		s.Run(tc.name, func() {
			acct := uncheckedAccount(bankAccount("swedbank", ibanSavings, "", "EUR", "Savings"), report.BankError,
				tc.raw)
			d := digest.Render(report.RunReport{Window: s.window(), Accounts: []report.AccountResult{acct}}, banks())

			s.Equal([]string{"", "⚠ Problems", tc.want}, d.Lines[1:])
		})
	}
}

// TestFreeTextFieldsAreSanitized pins that every interpolated free-text field, not only the
// description, is stripped of control, format and separator characters: the bank account name in
// the heading and in the unchecked line, and a Problem's reason. No line may contain a newline.
func (s *RenderSuite) TestFreeTextFieldsAreSanitized() {
	const hostileName = "Main\n\u202eNIAM"

	rep := report.RunReport{
		Window: s.window(),
		Accounts: []report.AccountResult{
			s.checkedAccount(bankAccount("swedbank", ibanMain, "", "EUR", hostileName), 2,
				s.missing("2026-09-20", "-4.50", "EUR", "COFFEE SHOP")),
			uncheckedAccount(bankAccount("revolut", ibanRevolut, "", "EUR", hostileName), report.RateLimited, ""),
		},
		Problems: []report.Problem{{Scope: "firefly", Reason: "bad\ngateway\u202e\u2028"}},
	}

	d := digest.Render(rep, banks())

	s.Equal([]string{
		"",
		"⚠ Problems",
		"- firefly: badgateway",
		"- revolut LT99…0001 (MainNIAM): unchecked — rate limited",
		"",
		"Missing in Firefly III",
		"Swedbank · LT12…3456 · MainNIAM (EUR)",
		"- 2026-09-20  -4.50 EUR  COFFEE SHOP",
	}, d.Lines[1:])

	for _, line := range d.Lines {
		s.NotContains(line, "\n")
	}

	s.NotContains(d.Text(), "\u202e")
}

// TestProblemLines pins the Problems section: run-level problems first as `- <scope>: <reason>`
// sorted by scope and then reason, then one `- <bank key> <masked id> (<account name>): unchecked —
// <reason text>[: <detail>]` line per unchecked account in account order. Ambiguous, with an empty
// Detail, appends the candidate Firefly III ids.
func (s *RenderSuite) TestProblemLines() {
	ambiguous := uncheckedAccount(bankAccount("revolut", ibanRevolut, "", "USD", "Revolut USD"),
		report.Ambiguous, "")
	ambiguous.Mapping.Status = accountmap.Ambiguous
	ambiguous.Mapping.Candidates = []firefly.Account{
		{ID: "21", Name: "Revolut USD", IBAN: ibanRevolut, Currency: "USD", DecimalPlaces: 2, Active: true},
		{ID: "22", Name: "Revolut USD old", IBAN: ibanRevolut, Currency: "USD", DecimalPlaces: 2, Active: true},
	}

	unmapped := uncheckedAccount(bankAccount("wise", "", hashWise, "JPY", "Yen"), report.Unmapped, "")
	unmapped.Mapping.Status = accountmap.Unmapped

	rep := report.RunReport{
		Window: s.window(),
		Accounts: []report.AccountResult{
			uncheckedAccount(bankAccount("swedbank", ibanSavings, "", "EUR", "Savings"), report.RateLimited,
				"provider returned 429"),
			unmapped,
			ambiguous,
			uncheckedAccount(bankAccount("seb", ibanLow, "", "EUR", "Old"), report.OverrideTargetMissing, "#7"),
			uncheckedAccount(bankAccount("swedbank", ibanMain, "", "EUR", "Main"), report.BankError,
				"HTTP 503 from provider"),
		},
		Problems: []report.Problem{
			{Scope: "luminor", Reason: "not authorized"},
			{Scope: "firefly", Reason: "unreachable"},
			{Scope: "firefly", Reason: "timeout"},
		},
	}

	d := digest.Render(rep, banks())

	s.Equal([]string{
		"",
		"⚠ Problems",
		"- firefly: timeout",
		"- firefly: unreachable",
		"- luminor: not authorized",
		"- revolut LT99…0001 (Revolut USD): unchecked — ambiguous mapping (Firefly #21, #22)",
		"- seb LT11…0002 (Old): unchecked — override target not found: #7",
		"- swedbank LT12…3456 (Main): unchecked — bank error: HTTP 503 from provider",
		"- swedbank LT12…7890 (Savings): unchecked — rate limited: provider returned 429",
		"- wise hash:ab12…ef12 (Yen): unchecked — no Firefly III account mapped",
	}, d.Lines[1:])
}

// TestNoUnmaskedIBAN pins FR-036 over whole digests: no IBAN used anywhere in the fixtures (bank
// accounts, Firefly III accounts, ambiguous candidates, description and detail text) appears unmasked in the
// subject or the text.
func (s *RenderSuite) TestNoUnmaskedIBAN() {
	withIBANText := s.checkedAccount(bankAccount("revolut", ibanLow, "", "EUR", "Low"), 2,
		s.missing("2026-09-20", "-10.00", "EUR", "TRANSFER TO "+ibanInText))
	withIBANDetail := uncheckedAccount(bankAccount("swedbank", ibanSavings, "", "EUR", "Savings"), report.BankError,
		"rejected account "+ibanInText)
	withSpacedIBAN := s.checkedAccount(bankAccount("revolut", ibanLow, "", "EUR", "Low"), 2,
		s.missing("2026-09-20", "-10.00", "EUR", "TRANSFER TO "+ibanSpaced))
	withSpacedDetail := uncheckedAccount(bankAccount("swedbank", ibanSavings, "", "EUR", "Savings"), report.BankError,
		"rejected account "+ibanSpaced)

	cases := []goldenCase{
		{name: "full", report: s.fullReport()},
		{name: "missing only", report: s.missingOnlyReport()},
		{name: "consent only", report: s.consentOnlyReport()},
		{
			name:   "iban in text",
			report: report.RunReport{Window: s.window(), Accounts: []report.AccountResult{withIBANText}},
		},
		{
			name:   "iban in unchecked detail",
			report: report.RunReport{Window: s.window(), Accounts: []report.AccountResult{withIBANDetail}},
		},
		{
			name:   "spaced iban in text",
			report: report.RunReport{Window: s.window(), Accounts: []report.AccountResult{withSpacedIBAN}},
		},
		{
			name:   "spaced iban in unchecked detail",
			report: report.RunReport{Window: s.window(), Accounts: []report.AccountResult{withSpacedDetail}},
		},
	}

	for i := range cases {
		tc := &cases[i]
		s.Run(tc.name, func() {
			d := digest.Render(tc.report, banks())

			for _, iban := range []string{ibanMain, ibanSavings, ibanRevolut, ibanLow, ibanInText, ibanSpaced} {
				s.NotContains(d.Text(), iban)
				s.NotContains(d.Subject, iban)
			}

			s.NotContains(d.Text(), hashWise)
		})
	}
}

// renderLine renders tc.missing as the only missing transaction of one account and returns its
// line, after checking the digest has exactly the expected shape around it.
func (s *RenderSuite) renderLine(tc *lineCase) string {
	acct := s.checkedAccount(bankAccount("swedbank", ibanMain, "", tc.missing.Tx.Amount.Currency, "Main"), tc.places,
		tc.missing)
	if tc.unmapped {
		acct.Mapping.Firefly = nil
	}

	d := digest.Render(report.RunReport{Window: s.window(), Accounts: []report.AccountResult{acct}}, banks())

	s.Require().Len(d.Lines, 5, "header, blank, section, heading, line")

	return d.Lines[4]
}

// assertGolden compares got with the golden file name.golden, first rewriting that file when
// UPDATE_GOLDEN=1 is set. The variable is read here, per call, rather than through a flag, which
// would need a package-level variable.
func (s *RenderSuite) assertGolden(name, got string) {
	path := filepath.Clean(filepath.Join(goldenDir, name+".golden"))

	if os.Getenv("UPDATE_GOLDEN") == "1" {
		s.Require().NoError(os.WriteFile(path, []byte(got), 0o644))
	}

	want, err := os.ReadFile(path)
	s.Require().NoError(err, "read golden %s", path)
	s.Equal(string(want), got, "golden %s", path)
}

// fullReport exercises every section and both hint kinds; testdata/digest/full.golden is its
// digest. Accounts, missing lines and consent warnings are deliberately out of order.
func (s *RenderSuite) fullReport() report.RunReport {
	pending := s.missing("2026-09-21", "-4.50", "EUR", "COFFEE SHOP")
	pending.Pending = true

	taken := s.missing("2026-09-15", "-4.50", "EUR", "COFFEE SHOP")
	taken.Hint = &reconcile.Hint{GroupID: "7", Date: s.date("2026-09-13"), Kind: reconcile.Taken}

	nearMiss := s.missing("2026-09-10", "-25.00", "EUR", "CITY PARKING")
	nearMiss.Hint = &reconcile.Hint{GroupID: "9", Date: s.date("2026-09-15"), Kind: reconcile.NearMiss}

	last := s.missing("2026-08-24", "-12.00", "EUR", "STREAMING SERVICE")
	last.LastReminder = true

	ambiguous := uncheckedAccount(bankAccount("revolut", ibanRevolut, "", "USD", "Revolut USD"),
		report.Ambiguous, "")
	ambiguous.Mapping.Status = accountmap.Ambiguous
	ambiguous.Mapping.Candidates = []firefly.Account{
		{ID: "21", Name: "Revolut USD", IBAN: ibanRevolut, Currency: "USD", DecimalPlaces: 2, Active: true},
		{ID: "22", Name: "Revolut USD old", IBAN: ibanRevolut, Currency: "USD", DecimalPlaces: 2, Active: true},
	}

	return report.RunReport{
		Window: s.window(),
		Accounts: []report.AccountResult{
			s.checkedAccount(bankAccount("swedbank", ibanMain, "", "EUR", "Main"), 2,
				nearMiss, taken, s.missing("2026-09-19", "-63.12", "EUR", "SUPERMARKET"), pending),
			uncheckedAccount(bankAccount("swedbank", ibanSavings, "", "EUR", "Savings"), report.RateLimited,
				"provider returned 429"),
			ambiguous,
			s.checkedAccount(bankAccount("revolut", ibanRevolut, "", "EUR", "Revolut EUR"), 2, last),
		},
		ConsentWarnings: []report.ConsentWarning{
			{BankKey: "seb", ValidUntil: validUntil(2026, 9, 27), DaysLeft: 5},
		},
		Problems: []report.Problem{{Scope: "luminor", Reason: "not authorized"}},
	}
}

// missingOnlyReport has missing transactions only: two accounts, one identified by hash with a
// zero-decimal currency, and lines that tie on date and on amount; testdata/digest/missing_only.golden
// is its digest.
func (s *RenderSuite) missingOnlyReport() report.RunReport {
	wise := s.checkedAccount(bankAccount("wise", "", hashWise, "JPY", "Yen"), 0,
		s.missing("2026-09-05", "-1200", "JPY", "RAMEN BAR"))
	wise.Mapping.Status = accountmap.Override

	return report.RunReport{
		Window: s.window(),
		Accounts: []report.AccountResult{
			wise,
			s.checkedAccount(bankAccount("swedbank", ibanMain, "", "EUR", "Main"), 2,
				s.missing("2026-09-20", "20.00", "EUR", "REFUND FROM "+ibanSpaced),
				s.missing("2026-09-20", "-4.50", "EUR", "COFFEE SHOP"),
				s.missing("2026-09-20", "-4.50", "EUR", "BAKERY"),
			),
			s.checkedAccount(bankAccount("revolut", ibanRevolut, "", "EUR", "Revolut EUR"), 2),
		},
	}
}

// consentOnlyReport has consent warnings only, plus a clean account and an excluded one with a
// leftover missing transaction that must not show; testdata/digest/consent_only.golden is its
// digest.
func (s *RenderSuite) consentOnlyReport() report.RunReport {
	return report.RunReport{
		Window: s.window(),
		Accounts: []report.AccountResult{
			s.checkedAccount(bankAccount("swedbank", ibanMain, "", "EUR", "Main"), 2),
			s.excludedAccount(bankAccount("seb", ibanLow, "", "EUR", "Old"),
				s.missing("2026-09-20", "-1.00", "EUR", "IGNORED")),
		},
		ConsentWarnings: []report.ConsentWarning{
			{BankKey: "swedbank", ValidUntil: validUntil(2026, 10, 1), DaysLeft: 9},
			{BankKey: "seb", ValidUntil: validUntil(2026, 9, 27), DaysLeft: 5},
		},
	}
}

// window is the default fixture window, 30 days ending 2026-09-22.
func (s *RenderSuite) window() civil.Range {
	return civil.Range{From: s.date("2026-08-24"), To: s.date("2026-09-22")}
}

// date parses a YYYY-MM-DD fixture date.
func (s *RenderSuite) date(v string) civil.Date {
	d, err := civil.ParseDate(v)
	s.Require().NoError(err)

	return d
}

// missing returns a booked missing transaction with no flags and no hint; checkedAccount fills in
// its Account.
func (s *RenderSuite) missing(date, amount, currency, description string) reconcile.Missing {
	a, err := money.ParseAmount(amount, currency)
	s.Require().NoError(err)

	return reconcile.Missing{Tx: bank.Transaction{
		Date:        s.date(date),
		Amount:      a,
		Status:      bank.Booked,
		Description: description,
	}}
}

// checkedAccount returns an automatically mapped account whose Firefly III account has the given
// precision and whose reconcile result reports missing, each attributed to acct.
func (s *RenderSuite) checkedAccount(
	acct bank.Account, places uint8, missing ...reconcile.Missing,
) report.AccountResult {
	for i := range missing {
		missing[i].Tx.Account = acct
	}

	return report.AccountResult{
		Mapping: accountmap.Mapping{
			Bank:   acct,
			Status: accountmap.Auto,
			Firefly: &firefly.Account{
				ID:            "3",
				Name:          acct.Name,
				IBAN:          acct.IBAN,
				Currency:      acct.Currency,
				DecimalPlaces: places,
				Active:        true,
			},
		},
		Result: reconcile.Result{Missing: missing},
	}
}

// uncheckedAccount returns an account reconciliation never ran against, for code.
func uncheckedAccount(acct bank.Account, code report.UncheckedCode, detail string) report.AccountResult {
	return report.AccountResult{
		Mapping:   accountmap.Mapping{Bank: acct, Status: accountmap.Auto},
		Unchecked: &report.Unchecked{Code: code, Detail: detail},
	}
}

// excludedAccount returns an excluded account carrying a leftover result, which a real run never
// produces; a non-empty one proves Render ignores it rather than happening to see nothing.
func (s *RenderSuite) excludedAccount(acct bank.Account, missing ...reconcile.Missing) report.AccountResult {
	result := s.checkedAccount(acct, 2, missing...)
	result.Mapping.Status = accountmap.Excluded
	result.Mapping.Firefly = nil

	return result
}

// bankAccount returns a bank account; UID is irrelevant to the digest and left empty.
func bankAccount(key, iban, hash, currency, name string) bank.Account {
	return bank.Account{BankKey: key, Hash: hash, IBAN: iban, Currency: currency, Name: name}
}

// banks is the banks: map every case renders against: one bank with a display name, the rest with
// only a name, and no entry for "unknownbank".
func banks() map[string]config.Bank {
	return map[string]config.Bank{
		"swedbank": {Name: "Swedbank AB", Country: "LT", Display: "Swedbank"},
		"revolut":  {Name: "Revolut", Country: "LT"},
		"wise":     {Name: "Wise", Country: "BE"},
		"seb":      {Name: "SEB", Country: "LT", Display: "SEB Bank"},
	}
}

// validUntil returns a consent expiry at midday UTC on the given date.
func validUntil(year int, month time.Month, day int) time.Time {
	return time.Date(year, month, day, 12, 0, 0, 0, time.UTC)
}
