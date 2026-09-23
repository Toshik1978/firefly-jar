package app

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/Toshik1978/firefly-jar/internal/digest"
	"github.com/Toshik1978/firefly-jar/internal/firefly"
	"github.com/Toshik1978/firefly-jar/internal/mapping"
	"github.com/Toshik1978/firefly-jar/internal/redact"
)

// unmappedMark is the FIREFLY column's rendering for a mapping with no Firefly III account to show:
// ambiguous and unmapped accounts, and any status this build does not otherwise recognize
// (contracts/cli.md).
const unmappedMark = "—"

// accountsHeader is the accounts table's fixed header (contracts/cli.md); --ids appends "HASH".
const accountsHeader = "BANK\tACCOUNT\tNAME\tCUR\tSTATUS\tFIREFLY"

// accountsIDsHeader is the trailing column --ids adds.
const accountsIDsHeader = "HASH"

// Accounts renders the read-only `accounts [--ids]` table (FR-017, contracts/cli.md, spec.md US3).
// Bank accounts come from deps.State's saved session, exactly what check would use, so the command makes
// no bank-provider call of its own (constitution §I); only Firefly III's asset accounts are listed. Rows
// are grouped by configured bank key in sorted order, then by the session's own account order, the same
// order check.go's reconcileAll visits (ruling on fj-xwu.5.3).
//
// The returned error is set only when Firefly III itself could not be listed, a run-level problem the
// caller reports on stderr; nothing else is printed then, and no table is written. A configured bank with
// no usable saved session — none at all, or one that saw no accounts — is not an error either: its row is
// left out of the table (there is nothing to show), but it is named in problems, one line per such bank
// (fix round 1 on fj-xwu.5.4: the setup check must not stay quiet about it), formatted exactly as the
// caller should print each on its own stderr line. A mapping that leaves a non-excluded account unmapped,
// or any problems, force exit 2 rather than 0 — never an error return, only the code.
func Accounts(ctx context.Context, deps Deps, ids bool) (int, []string, error) {
	ffAccounts, err := deps.Firefly.ListAccounts(ctx)
	if err != nil {
		return exitError, nil, fmt.Errorf("list firefly accounts: %w", err)
	}

	rows, complete, problems := accountRows(deps, ffAccounts)
	writeAccountsTable(deps.Stdout, rows, ids)

	if !complete {
		return exitError, problems, nil
	}

	return exitOK, problems, nil
}

// accountRow is one line of the accounts table.
type accountRow struct {
	bank    string
	account string
	name    string
	cur     string
	status  string
	firefly string
	hash    string
}

// accountRows resolves every configured bank's saved accounts against ffAccounts and turns each mapping
// into one accountRow, reporting whether every non-excluded account resolved to auto or override
// (contracts/cli.md's exit rule) and naming every configured bank whose saved session could not be used
// at all (fix round 1 on fj-xwu.5.4), the same two reasons check.go's usableSession reports.
func accountRows(deps Deps, ffAccounts []firefly.Account) ([]accountRow, bool, []string) {
	complete := true

	var (
		rows     []accountRow
		problems []string
	)

	for _, key := range configuredBanks(deps.Config.Banks) {
		session, ok := deps.State.Sessions[key]

		reason, usable := sessionProblem(session, ok, key)
		if !usable {
			complete = false
			problems = append(problems, key+": "+reason)

			continue
		}

		mappings := mapping.Resolve(bankAccounts(key, &session), ffAccounts, deps.Config.Accounts)
		for i := range mappings {
			rows = append(rows, newAccountRow(key, mappings[i]))

			status := mappings[i].Status
			if status != mapping.Excluded && status != mapping.Auto && status != mapping.Override {
				complete = false
			}
		}
	}

	return rows, complete, problems
}

// newAccountRow renders one resolved mapping as a table row. ACCOUNT is the masked IBAN when the bank
// account has one, or its masked hash otherwise (a card with no IBAN, contracts/cli.md).
func newAccountRow(bankKey string, m mapping.Mapping) accountRow {
	account := redact.MaskHash(m.Bank.Hash)
	if m.Bank.IBAN != "" {
		account = redact.MaskIBAN(m.Bank.IBAN)
	}

	return accountRow{
		bank:    bankKey,
		account: account,
		name:    m.Bank.Name,
		cur:     m.Bank.Currency,
		status:  m.Status.String(),
		firefly: fireflyColumn(m),
		hash:    m.Bank.Hash,
	}
}

// fireflyColumn renders the FIREFLY column: "#<id> <name>" for an auto or override match, the sorted
// candidate ids for an ambiguous one, or unmappedMark for anything else (contracts/cli.md).
func fireflyColumn(m mapping.Mapping) string {
	if m.Firefly != nil {
		return "#" + m.Firefly.ID + " " + m.Firefly.Name
	}

	if m.Status == mapping.Ambiguous {
		ids := make([]string, 0, len(m.Candidates))
		for _, c := range m.Candidates {
			ids = append(ids, "#"+c.ID)
		}

		return strings.Join(ids, ", ")
	}

	return unmappedMark
}

// writeAccountsTable writes rows to w as a text/tabwriter table, with the header contracts/cli.md fixes
// and, when ids is set, a trailing HASH column carrying each account's full, unmasked hash. Every cell
// passes through digest.Strip first, the same sanitizing the digest applies.
func writeAccountsTable(w io.Writer, rows []accountRow, ids bool) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)

	header := accountsHeader
	if ids {
		header += "\t" + accountsIDsHeader
	}

	fmt.Fprintln(tw, header)

	for _, r := range rows {
		cols := []string{r.bank, r.account, r.name, r.cur, r.status, r.firefly}
		if ids {
			cols = append(cols, r.hash)
		}

		// Names, currencies and hashes come from the bank and Firefly III: a tab would shift the
		// columns, a line break would add a fake row and a bidi override would reorder the text.
		for i := range cols {
			cols[i] = digest.Strip(cols[i])
		}

		fmt.Fprintln(tw, strings.Join(cols, "\t"))
	}

	_ = tw.Flush()
}
