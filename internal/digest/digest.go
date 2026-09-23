// Package digest renders one run's report.RunReport as the plain-text reminder digest every
// notifier sends (FR-025, FR-025a, FR-036, contracts/digest.md). Render is a pure function: it
// reads no clock, does no I/O and never mutates the report it is given, so the same report always
// yields the same digest for every recipient.
package digest

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/Toshik1978/firefly-jar/internal/accountmap"
	"github.com/Toshik1978/firefly-jar/internal/config"
	"github.com/Toshik1978/firefly-jar/internal/redact"
	"github.com/Toshik1978/firefly-jar/internal/report"
)

// Section titles, in the order the digest shows them.
const (
	titleProblems = "⚠ Problems"
	titleConsent  = "⏰ Consent"
	titleMissing  = "Missing in Firefly III"
)

// Digest is one rendered reminder. Subject is the header line, which email uses as its subject;
// Lines holds every line of the body, header first, none of them containing a newline, so the
// Telegram notifier can split between any two of them.
type Digest struct {
	Subject string
	Lines   []string
}

// renderer carries what every section needs: the banks: map for display names and one Redactor,
// built per Render, that masks IBAN-shaped text inside free-text fields.
type renderer struct {
	banks    map[string]config.Bank
	redactor *redact.Redactor
}

// accountView is one non-excluded account with its masked identifier computed once, since both
// the sort and every line naming the account use it.
type accountView struct {
	result   *report.AccountResult
	maskedID string
}

// Render builds the digest for r. banks supplies each bank key's display name; a key missing from
// it is shown as the key itself. Excluded accounts appear nowhere, and every section with nothing
// to show is left out entirely.
func Render(r report.RunReport, banks map[string]config.Bank) Digest {
	rn := renderer{banks: banks, redactor: redact.New()}
	accounts := sortedAccounts(r.Accounts)
	header := headerLine(r)

	lines := []string{header}
	lines = appendSection(lines, titleProblems, rn.problemLines(r.Problems, accounts))
	lines = appendSection(lines, titleConsent, consentLines(r.ConsentWarnings))
	lines = appendSection(lines, titleMissing, rn.missingLines(accounts))

	return Digest{Subject: header, Lines: lines}
}

// Text returns the whole digest as one string: the lines joined by newlines, ending in one.
func (d Digest) Text() string {
	return strings.Join(d.Lines, "\n") + "\n"
}

// headerLine renders the first line, which doubles as the subject. Its counts come from
// report.Summary, so an excluded account counts for nothing and an unchecked one only as
// unchecked.
func headerLine(r report.RunReport) string {
	sum := r.Summary()

	noun := "accounts"
	if sum.AccountsUnchecked == 1 {
		noun = "account"
	}

	return fmt.Sprintf("firefly-jar: %d missing, %d unchecked %s (window %s – %s)",
		sum.Missing, sum.AccountsUnchecked, noun, r.Window.From, r.Window.To)
}

// appendSection appends a blank separator line, title and body to lines, or nothing at all when
// body is empty.
func appendSection(lines []string, title string, body []string) []string {
	if len(body) == 0 {
		return lines
	}

	lines = append(lines, "", title)

	return append(lines, body...)
}

// sortedAccounts returns every non-excluded account of accounts in digest order: bank key, then
// masked identifier compared as text, then currency. The views point into accounts, which is
// never reordered itself.
func sortedAccounts(accounts []report.AccountResult) []accountView {
	views := make([]accountView, 0, len(accounts))

	for i := range accounts {
		a := &accounts[i]
		if a.Mapping.Status == accountmap.Excluded {
			continue
		}

		views = append(views, accountView{result: a, maskedID: maskedID(a.Mapping)})
	}

	slices.SortStableFunc(views, func(x, y accountView) int {
		bx, by := &x.result.Mapping.Bank, &y.result.Mapping.Bank

		return cmp.Or(
			cmp.Compare(bx.BankKey, by.BankKey),
			cmp.Compare(x.maskedID, y.maskedID),
			cmp.Compare(bx.Currency, by.Currency),
		)
	})

	return views
}

// maskedID identifies m's bank account without exposing it (FR-036): the masked IBAN, or the
// masked hash for an account the bank reports no IBAN for.
func maskedID(m accountmap.Mapping) string {
	if m.Bank.IBAN != "" {
		return redact.MaskIBAN(m.Bank.IBAN)
	}

	return redact.MaskHash(m.Bank.Hash)
}

// pluralDays renders n whole days, singular only for exactly one.
func pluralDays(n int) string {
	if n == 1 {
		return "1 day"
	}

	return fmt.Sprintf("%d days", n)
}
