package digest

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Toshik1978/firefly-jar/internal/report"
)

// problemLines renders the Problems section body: run-level problems sorted by scope and then
// reason, followed by one line per unchecked account in account order.
func (rn *renderer) problemLines(problems []report.Problem, accounts []accountView) []string {
	lines := make([]string, 0, len(problems))

	for _, p := range sortedProblems(problems) {
		lines = append(lines, fmt.Sprintf("- %s: %s", p.Scope, p.Reason))
	}

	for _, v := range accounts {
		if v.result.Unchecked != nil {
			lines = append(lines, rn.uncheckedLine(v))
		}
	}

	return lines
}

// uncheckedLine renders `- <bank key> <masked id> (<account name>): unchecked — <reason>`, then
// `: <detail>` when the detail survives sanitizing, then the candidate Firefly III ids of an
// ambiguous mapping.
func (rn *renderer) uncheckedLine(v accountView) string {
	a := v.result

	var b strings.Builder

	fmt.Fprintf(&b, "- %s %s (%s): unchecked — %s",
		strip(a.Mapping.Bank.BankKey), v.maskedID, strip(a.Mapping.Bank.Name), a.Unchecked.Code.Text())

	if detail := rn.text(a.Unchecked.Detail); detail != "" {
		b.WriteString(": " + detail)
	}

	if a.Unchecked.Code == report.Ambiguous && len(a.Mapping.Candidates) > 0 {
		ids := make([]string, 0, len(a.Mapping.Candidates))
		for i := range a.Mapping.Candidates {
			ids = append(ids, "#"+strip(a.Mapping.Candidates[i].ID))
		}

		b.WriteString(" (Firefly " + strings.Join(ids, ", ") + ")")
	}

	return b.String()
}

// sortedProblems returns problems sanitized and sorted by scope, then reason, so the section reads
// the same whatever order the run hit them in.
func sortedProblems(problems []report.Problem) []report.Problem {
	sorted := make([]report.Problem, 0, len(problems))
	for _, p := range problems {
		sorted = append(sorted, report.Problem{Scope: strip(p.Scope), Reason: strip(p.Reason)})
	}

	slices.SortStableFunc(sorted, func(x, y report.Problem) int {
		return cmp.Or(cmp.Compare(x.Scope, y.Scope), cmp.Compare(x.Reason, y.Reason))
	})

	return sorted
}

// consentLines renders the Consent section body, one line per warning sorted by bank key. The line
// always names the config key, since that is what `firefly-jar auth` takes, and shows ValidUntil's
// calendar date in its own Location: converting to the configured zone is the caller's job.
func consentLines(warnings []report.ConsentWarning) []string {
	sorted := slices.Clone(warnings)
	slices.SortStableFunc(sorted, func(x, y report.ConsentWarning) int {
		return cmp.Compare(x.BankKey, y.BankKey)
	})

	lines := make([]string, 0, len(sorted))
	for i := range sorted {
		w := &sorted[i]
		key := strip(w.BankKey)
		lines = append(lines, fmt.Sprintf("- %s: consent expires %s (%s) — run: firefly-jar auth %s",
			key, w.ValidUntil.Format(time.DateOnly), pluralDays(w.DaysLeft), key))
	}

	return lines
}
