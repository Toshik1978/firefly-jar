package app_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/stretchr/testify/suite"

	"github.com/Toshik1978/firefly-jar/internal/app"
	"github.com/Toshik1978/firefly-jar/internal/bank"
	"github.com/Toshik1978/firefly-jar/internal/civil"
	"github.com/Toshik1978/firefly-jar/internal/domain"
	"github.com/Toshik1978/firefly-jar/internal/redact"
	"github.com/Toshik1978/firefly-jar/internal/state"
)

// Anonymized fixture identifiers for AcceptanceUS3Suite (T069, spec.md User Story 3). One bank,
// us3Bank, carries seven accounts, one per mapping.Status the resolution order can produce plus the
// multi-currency case spec.md US3 scenario 2 calls out: us3Main (auto), us3Override (an IBAN that
// would auto-map, redirected by a hash override — scenario 4), us3MultiEUR/us3MultiUSD (one IBAN
// shared by two currencies, each resolving to its own currency's Firefly III account — scenario 2),
// us3Ambiguous (two active Firefly III candidates — scenario 3), us3Excluded (an accounts: exclude
// rule — scenario 5) and us3Unmapped (no rule, no unique match — scenario 6).
const (
	us3Bank        = "us3bank"
	us3BankName    = "US3 Bank"
	us3BankCountry = "LT"
	us3BankDisplay = "US3 Bank"

	us3SessionID = "00000000-0000-0000-0000-000000000401"

	us3MainUID      = "00000000-0000-0000-0000-000000000411"
	us3MainHash     = "hash-main-00000000000000000000000000000"
	us3MainIBAN     = "LT610000000000000101"
	us3MainName     = "Main"
	us3MainFFID     = "101"
	us3MainFFName   = "Main EUR"
	us3AutoCandFFID = "110"
	us3AutoCandName = "Auto Candidate"

	us3OverrideUID   = "00000000-0000-0000-0000-000000000412"
	us3OverrideHash  = "hash-override-0000000000000000000000000"
	us3OverrideIBAN  = "LT610000000000000110"
	us3OverrideName  = "Override"
	us3OverrideFFID  = "111"
	us3OverrideFF    = "Override Target"
	us3MultiIBAN     = "LT610000000000000120"
	us3MultiEURUID   = "00000000-0000-0000-0000-000000000413"
	us3MultiEURHash  = "hash-multieur-000000000000000000000000"
	us3MultiEURName  = "Multi EUR"
	us3MultiEURFFID  = "120"
	us3MultiEURFF    = "Multi EUR FF"
	us3MultiUSDUID   = "00000000-0000-0000-0000-000000000414"
	us3MultiUSDHash  = "hash-multiusd-000000000000000000000000"
	us3MultiUSDName  = "Multi USD"
	us3MultiUSDFFID  = "121"
	us3MultiUSDFF    = "Multi USD FF"
	us3AmbiguousIBAN = "LT610000000000000130"
	us3AmbiguousUID  = "00000000-0000-0000-0000-000000000415"
	us3AmbiguousHash = "hash-ambiguous-00000000000000000000000"
	us3AmbiguousName = "Ambiguous"
	us3AmbiguousFFA  = "130"
	us3AmbiguousFFB  = "131"
	us3AmbiguousFFAN = "Ambiguous A"
	us3AmbiguousFFBN = "Ambiguous B"
	us3ExcludedUID   = "00000000-0000-0000-0000-000000000416"
	us3ExcludedHash  = "hash-excluded-000000000000000000000000"
	us3ExcludedIBAN  = "LT610000000000000140"
	us3ExcludedName  = "Excluded"
	us3UnmappedUID   = "00000000-0000-0000-0000-000000000417"
	us3UnmappedHash  = "hash-unmapped-000000000000000000000000"
	us3UnmappedIBAN  = "LT610000000000000150"
	us3UnmappedName  = "Unmapped"

	us3FireflyToken = "test-firefly-token-0300"

	// us3Header is the accounts table header, without --ids (contracts/cli.md, shared with
	// accounts_test.go's acctHeader).
	us3Header = "BANK ACCOUNT NAME CUR STATUS FIREFLY"
)

// Anonymized fixture identifiers for the Independent Test flow (tasks.md Phase 5): a second,
// isolated bank with exactly the two accounts the Independent Test names, one auto-mapped by IBAN
// and one carrying no IBAN at all.
const (
	us3IndepBank       = "us3indepbank"
	us3IndepBankName   = "US3 Independent Bank"
	us3IndepSessionID  = "00000000-0000-0000-0000-000000000501"
	us3IndepAutoUID    = "00000000-0000-0000-0000-000000000511"
	us3IndepAutoHash   = "hash-indepauto-00000000000000000000000"
	us3IndepAutoIBAN   = "LT610000000000000201"
	us3IndepAutoName   = "Auto"
	us3IndepAutoFFID   = "201"
	us3IndepAutoFFName = "Auto FF"
	us3IndepCardUID    = "00000000-0000-0000-0000-000000000512"
	us3IndepCardHash   = "hash-indepcard-00000000000000000000000"
	us3IndepCardName   = "Card"
	us3IndepCardFFID   = "202"
	us3IndepCardFFName = "Card FF"
)

// us3FFAccount is one Firefly III asset account the fake serves from GET /accounts.
type us3FFAccount struct {
	id       string
	name     string
	iban     string
	currency string
}

// us3AccountsPage renders accounts as one Firefly III GET /accounts page (no meta block, the same
// single-page shape testdata/firefly/accounts_nometa.json uses).
func us3AccountsPage(accounts []us3FFAccount) map[string]any {
	data := make([]map[string]any, 0, len(accounts))

	for _, a := range accounts {
		data = append(data, map[string]any{
			"type": "accounts",
			"id":   a.id,
			"attributes": map[string]any{
				"active":                  true,
				"name":                    a.name,
				"account_role":            "defaultAsset",
				"currency_code":           a.currency,
				"currency_decimal_places": 2,
				"iban":                    a.iban,
			},
		})
	}

	return map[string]any{"data": data}
}

// us3Entry is one single-split Firefly III transaction group on some Firefly III account, in the
// currency that account carries. Amount is positive, as Firefly III always renders it; kind is
// withdrawal (the account is the source) or deposit (the account is the destination).
type us3Entry struct {
	group       string
	kind        string
	date        string
	amount      string
	currency    string
	description string
}

// us3GroupsPage renders entries as one Firefly III transactions list page relative to accountID.
func us3GroupsPage(accountID string, entries []us3Entry) map[string]any {
	data := make([]map[string]any, 0, len(entries))

	for _, e := range entries {
		src, dst := accountID, "900"
		if e.kind == "deposit" {
			src, dst = "901", accountID
		}

		data = append(data, map[string]any{
			"type": "transactions",
			"id":   e.group,
			"attributes": map[string]any{
				"group_title": nil,
				"transactions": []map[string]any{{
					"transaction_journal_id": e.group + "0",
					"type":                   e.kind,
					"date":                   e.date + "T12:00:00+03:00",
					"currency_code":          e.currency,
					"foreign_currency_code":  nil,
					"amount":                 e.amount,
					"foreign_amount":         nil,
					"description":            e.description,
					"source_id":              src,
					"destination_id":         dst,
				}},
			},
		})
	}

	return map[string]any{"data": data}
}

// us3Firefly is an httptest Firefly III serving a fixed accounts fixture and, per Firefly III
// account id, a scripted list of transaction groups.
type us3Firefly struct {
	srv          *httptest.Server
	accountsBody []byte
	entries      map[string][]us3Entry
}

// serveAccounts serves GET /accounts: the fixture.
func (f *us3Firefly) serveAccounts(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(f.accountsBody)
}

// serveTransactions serves GET /accounts/{id}/transactions as one page (no meta block), the
// scripted entries for id, or none if the case never gave it any.
func (f *us3Firefly) serveTransactions(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	body, err := json.Marshal(us3GroupsPage(id, f.entries[id]))
	if err != nil {
		http.Error(w, "encode", http.StatusInternalServerError)

		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

// us3Harness is one AcceptanceUS3Suite case: a temp dir holding the config, state and the private
// key fixture; the fake Firefly III; the fake bank provider through a cliFactory (reused from
// cli_test.go); the fixed clock; and the two console streams.
type us3Harness struct {
	configPath string
	statePath  string
	env        map[string]string
	firefly    *us3Firefly
	factory    *cliFactory
	stdout     *bytes.Buffer
	stderr     *bytes.Buffer
	clock      time.Time
}

// run resets the console buffers, then invokes app.RunEnv with args and the harness's Env, so a
// case chaining several commands (the Independent Test's accounts, accounts, check) reads each
// command's own output in turn.
func (h *us3Harness) run(args ...string) int {
	h.stdout.Reset()
	h.stderr.Reset()

	return app.RunEnv(args, app.Env{
		Stdin:     strings.NewReader(""),
		Stdout:    h.stdout,
		Stderr:    h.stderr,
		Getenv:    func(key string) string { return h.env[key] },
		Now:       func() time.Time { return h.clock },
		Transport: h.firefly.srv.Client().Transport,
		Factory:   h.factory.build,
	})
}

// outputLines splits h's stdout into its non-empty lines.
func (h *us3Harness) outputLines() []string {
	out := strings.TrimRight(h.stdout.String(), "\n")
	if out == "" {
		return nil
	}

	return strings.Split(out, "\n")
}

// AcceptanceUS3Suite is the US3 acceptance test (T069, spec.md User Story 3, tasks.md Phase 5): it
// drives the real CLI path app.RunEnv, through `accounts` and `check --stdout`, end to end, over
// one bank (us3Bank) whose seven accounts cover every spec.md US3 acceptance scenario, plus the
// Independent Test's own two-account flow on a second, isolated bank. It reuses cli_test.go's
// cliFactory and check_test.go's fakeProvider and eventLog, so a defect this test finds is a defect
// in mapping.go, check.go, check_account.go, accounts.go or digest.go, never in a test-only fake.
type AcceptanceUS3Suite struct {
	suite.Suite
}

// TestScenario1AnAutoMappedAccountIsShownAutoMapped covers spec.md US3 acceptance scenario 1: a
// bank account whose IBAN and currency equal those of exactly one Firefly III asset account is
// shown as auto-mapped by the accounts command.
func (s *AcceptanceUS3Suite) TestScenario1AnAutoMappedAccountIsShownAutoMapped() {
	h := s.newHarness(nil, nil, nil, nil)

	code := h.run("accounts", "--config", h.configPath)

	s.Equal(2, code, "the fixture's ambiguous and unmapped accounts still fail the setup check")

	lines := h.outputLines()
	s.Require().NotEmpty(lines)
	s.Equal(strings.Fields(us3Header), splitColumns(lines[0]))

	row := s.findRow(lines, redact.MaskIBAN(us3MainIBAN))
	s.Require().NotEmpty(row, "the auto-mapped account's row is printed")
	s.Equal(
		[]string{
			us3Bank,
			redact.MaskIBAN(us3MainIBAN),
			us3MainName,
			"EUR",
			"auto",
			"#" + us3MainFFID + " " + us3MainFFName,
		},
		row,
	)
}

// TestScenario2MultiCurrencyAccountsMapToTheirOwnCurrencysFireflyAccount covers spec.md US3
// acceptance scenario 2: a multi-currency bank with EUR and USD accounts under one IBAN, and
// Firefly III asset accounts with that IBAN in EUR and USD, each maps to the Firefly III account of
// its own currency. Proven end to end: the EUR account's unentered transaction is reported missing
// against the EUR Firefly III account, and the USD account's transaction, entered on the USD
// Firefly III account, is not reported at all — which a currency mix-up in mapping.Resolve or
// checkAccount would fail, since the USD account would then compare against the EUR account's
// (empty) transactions and wrongly report its own transaction as missing too.
func (s *AcceptanceUS3Suite) TestScenario2MultiCurrencyAccountsMapToTheirOwnCurrencysFireflyAccount() {
	h := s.newHarness(
		[]bank.Transaction{s.bankTx("2026-09-19", "-50.00", "EUR", "ref-eur", "ANON MULTI EUR")},
		nil,
		[]bank.Transaction{s.bankTx("2026-09-20", "-75.00", "USD", "ref-usd", "ANON MULTI USD")},
		[]us3Entry{
			{
				group:       "601",
				kind:        "withdrawal",
				date:        "2026-09-21",
				amount:      "75.00",
				currency:    "USD",
				description: "FF MULTI USD",
			},
		},
	)

	code := h.run("accounts", "--config", h.configPath)
	s.Equal(2, code)

	lines := h.outputLines()
	s.assertRowByAccountAndName(lines, us3MultiIBAN, us3MultiEURName,
		[]string{
			us3Bank, redact.MaskIBAN(us3MultiIBAN), us3MultiEURName, "EUR", "auto",
			"#" + us3MultiEURFFID + " " + us3MultiEURFF,
		})
	s.assertRowByAccountAndName(lines, us3MultiIBAN, us3MultiUSDName,
		[]string{
			us3Bank, redact.MaskIBAN(us3MultiIBAN), us3MultiUSDName, "USD", "auto",
			"#" + us3MultiUSDFFID + " " + us3MultiUSDFF,
		})

	code = h.run("check", "--stdout", "--config", h.configPath)
	s.Equal(2, code, "the fixture's ambiguous and unmapped accounts still make this run fail")
	s.Empty(h.stderr.String())

	out := h.stdout.String()
	s.Contains(out, us3BankDisplay+" · "+redact.MaskIBAN(us3MultiIBAN)+" · "+us3MultiEURName+" (EUR)",
		"the EUR account's own heading is shown")
	s.Contains(out, "- 2026-09-19  -50.00 EUR  ANON MULTI EUR", "the EUR transaction is reported missing")
	s.NotContains(out, us3MultiUSDName+" (USD)",
		"the USD account has nothing missing, so its heading is never shown")
	s.NotContains(out, "ANON MULTI USD", "the USD transaction was entered, so it is never listed")
}

// TestScenario3AnAmbiguousMappingIsUncheckedWithExit2 covers spec.md US3 acceptance scenario 3: two
// Firefly III asset accounts sharing one IBAN and currency leave the bank account unchecked with
// reason "ambiguous mapping", naming both candidates, and the run ends with the "check failed"
// outcome.
func (s *AcceptanceUS3Suite) TestScenario3AnAmbiguousMappingIsUncheckedWithExit2() {
	h := s.newHarness(nil, nil, nil, nil)

	code := h.run("check", "--stdout", "--config", h.configPath)

	s.Equal(2, code)
	s.Empty(h.stderr.String())
	s.Contains(h.stdout.String(),
		"- "+us3Bank+" "+redact.MaskIBAN(us3AmbiguousIBAN)+" ("+us3AmbiguousName+
			"): unchecked — ambiguous mapping (Firefly #"+us3AmbiguousFFA+", #"+us3AmbiguousFFB+")")
}

// TestScenario4AConfiguredOverrideTakesPrecedenceOverIBANMatching covers spec.md US3 acceptance
// scenario 4: us3Override's IBAN and currency match Firefly III account #110 uniquely — automatic
// resolution alone would map it there — but a configured accounts: hash rule redirects it to
// account #111, and the accounts command shows that override, not the automatic match.
func (s *AcceptanceUS3Suite) TestScenario4AConfiguredOverrideTakesPrecedenceOverIBANMatching() {
	h := s.newHarness(nil, nil, nil, nil)

	code := h.run("accounts", "--config", h.configPath)
	s.Equal(2, code)

	lines := h.outputLines()
	row := s.findRow(lines, redact.MaskIBAN(us3OverrideIBAN))
	s.Require().NotEmpty(row)
	s.Equal(
		[]string{
			us3Bank, redact.MaskIBAN(us3OverrideIBAN), us3OverrideName, "EUR", "override",
			"#" + us3OverrideFFID + " " + us3OverrideFF,
		},
		row,
		"the override target (#111) wins, not the automatic IBAN match (#110)",
	)
}

// TestScenario5AnExcludedAccountIsNeitherCheckedNorReported covers spec.md US3 acceptance scenario
// 5: an account an accounts: exclude rule names is not checked (the fake bank provider never sees
// it) and never appears in the digest, even though the run reports other accounts as unchecked.
func (s *AcceptanceUS3Suite) TestScenario5AnExcludedAccountIsNeitherCheckedNorReported() {
	h := s.newHarness(nil, nil, nil, nil)

	code := h.run("check", "--stdout", "--config", h.configPath)

	s.Equal(2, code, "the fixture's ambiguous and unmapped accounts still make this run fail")
	s.Empty(h.stderr.String())

	out := h.stdout.String()
	s.NotContains(out, us3ExcludedName, "an excluded account is never named in the digest")
	s.NotContains(out, redact.MaskIBAN(us3ExcludedIBAN), "an excluded account's identifier never appears")
	s.NotContains(out, us3ExcludedIBAN)

	calls := h.factory.provider.recorded()
	for i := range calls {
		s.NotEqual(us3ExcludedUID, calls[i].account.UID, "the excluded account is never asked for its transactions")
	}
}

// TestScenario6AnUnmappedAccountIsUncheckedWithReasonAndExitCheckFailed covers spec.md US3
// acceptance scenario 6: a bank account that is neither mapped nor excluded is listed as unchecked
// with reason "no Firefly III account mapped", and the run ends with the "check failed" outcome.
func (s *AcceptanceUS3Suite) TestScenario6AnUnmappedAccountIsUncheckedWithReasonAndExitCheckFailed() {
	h := s.newHarness(nil, nil, nil, nil)

	code := h.run("check", "--stdout", "--config", h.configPath)

	s.Equal(2, code, `an unmapped, non-excluded account ends the run with the "check failed" outcome`)
	s.Empty(h.stderr.String())
	s.Contains(
		h.stdout.String(),
		"- "+us3Bank+" "+redact.MaskIBAN(
			us3UnmappedIBAN,
		)+" ("+us3UnmappedName+"): unchecked — no Firefly III account mapped",
	)
}

// TestIndependentTestAccountsThenHashOverrideThenCheckCoversBoth covers the Phase 5 Independent
// Test verbatim (tasks.md): two bank accounts, one auto-mapped by IBAN and one without an IBAN.
// `accounts` first shows `auto` and `unmapped` with exit 2; adding a `hash` override then makes
// both `override`-or-`auto` mapped with exit 0; `check` then checks both (a bank-provider call for
// each), where before the override the unmapped account cost no call at all.
func (s *AcceptanceUS3Suite) TestIndependentTestAccountsThenHashOverrideThenCheckCoversBoth() {
	h := s.newIndepHarness()

	code := h.run("accounts", "--config", h.configPath)
	s.Equal(2, code, "the unmapped card account fails the setup check")

	lines := h.outputLines()
	s.Require().Len(lines, 3, "one header line plus the two accounts")
	s.Equal(
		[]string{
			us3IndepBank, redact.MaskIBAN(us3IndepAutoIBAN), us3IndepAutoName, "EUR", "auto",
			"#" + us3IndepAutoFFID + " " + us3IndepAutoFFName,
		},
		splitColumns(lines[1]),
	)
	s.Equal(
		[]string{us3IndepBank, redact.MaskHash(us3IndepCardHash), us3IndepCardName, "EUR", "unmapped", "—"},
		splitColumns(lines[2]),
	)

	s.writeSecret(h.configPath, s.indepConfigYAML(filepath.Dir(h.configPath), h.firefly.srv.URL, true))

	code = h.run("accounts", "--config", h.configPath)
	s.Equal(0, code, "every account is now mapped")

	lines = h.outputLines()
	s.Require().Len(lines, 3)
	s.Equal("auto", splitColumns(lines[1])[4])
	s.Equal(
		[]string{
			us3IndepBank, redact.MaskHash(us3IndepCardHash), us3IndepCardName, "EUR", "override",
			"#" + us3IndepCardFFID + " " + us3IndepCardFFName,
		},
		splitColumns(lines[2]),
	)

	code = h.run("check", "--stdout", "--config", h.configPath)
	s.Equal(0, code, "both accounts are mapped and nothing is missing")
	s.Empty(h.stdout.String())
	s.Empty(h.stderr.String())

	calls := h.factory.provider.recorded()
	s.Require().Len(calls, 2, "check now asks the bank for both accounts")

	uids := make([]string, 0, len(calls))
	for i := range calls {
		uids = append(uids, calls[i].account.UID)
	}

	s.ElementsMatch([]string{us3IndepAutoUID, us3IndepCardUID}, uids)
}

// findRow returns the columns of the one line in lines whose masked identifier equals want, or nil
// if none matches.
func (s *AcceptanceUS3Suite) findRow(lines []string, want string) []string {
	s.T().Helper()

	for _, line := range lines[1:] {
		cols := splitColumns(line)
		if len(cols) > 1 && cols[1] == want {
			return cols
		}
	}

	return nil
}

// assertRowByAccountAndName asserts that lines contains exactly one row whose masked ACCOUNT
// column is iban's mask and whose NAME column is name, equal to want.
func (s *AcceptanceUS3Suite) assertRowByAccountAndName(lines []string, iban, name string, want []string) {
	s.T().Helper()

	masked := redact.MaskIBAN(iban)

	for _, line := range lines[1:] {
		cols := splitColumns(line)
		if len(cols) > 2 && cols[1] == masked && cols[2] == name {
			s.Equal(want, cols)

			return
		}
	}

	s.Fail("no row found", "account %s name %s in %v", masked, name, lines)
}

// newHarness builds us3Bank's fixture: seven accounts (main, override, the two multi-currency
// accounts, ambiguous, excluded and unmapped), the matching Firefly III asset accounts, and one
// accounts: override each for us3Override (a hash rule to #111, redirecting it away from its
// automatic match #110) and us3Excluded (an exclude rule). multiEURTxs/multiEUREntries and
// multiUSDTxs/multiUSDEntries script the multi-currency scenario's own bank transactions and
// Firefly III entries; every other account is scripted with none.
func (s *AcceptanceUS3Suite) newHarness(
	multiEURTxs []bank.Transaction, multiEUREntries []us3Entry,
	multiUSDTxs []bank.Transaction, multiUSDEntries []us3Entry,
) *us3Harness {
	s.T().Helper()

	ffAccounts := []us3FFAccount{
		{id: us3MainFFID, name: us3MainFFName, iban: us3MainIBAN, currency: "EUR"},
		{id: us3AutoCandFFID, name: us3AutoCandName, iban: us3OverrideIBAN, currency: "EUR"},
		{id: us3OverrideFFID, name: us3OverrideFF, iban: "", currency: "EUR"},
		{id: us3MultiEURFFID, name: us3MultiEURFF, iban: us3MultiIBAN, currency: "EUR"},
		{id: us3MultiUSDFFID, name: us3MultiUSDFF, iban: us3MultiIBAN, currency: "USD"},
		{id: us3AmbiguousFFA, name: us3AmbiguousFFAN, iban: us3AmbiguousIBAN, currency: "GBP"},
		{id: us3AmbiguousFFB, name: us3AmbiguousFFBN, iban: us3AmbiguousIBAN, currency: "GBP"},
	}

	body, err := json.Marshal(us3AccountsPage(ffAccounts))
	s.Require().NoError(err)

	ff := &us3Firefly{
		accountsBody: body,
		entries:      map[string][]us3Entry{us3MultiEURFFID: multiEUREntries, us3MultiUSDFFID: multiUSDEntries},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/accounts", ff.serveAccounts)
	mux.HandleFunc("GET /api/v1/accounts/{id}/transactions", ff.serveTransactions)

	ff.srv = httptest.NewServer(mux)
	s.T().Cleanup(ff.srv.Close)

	dir := s.T().TempDir()

	key, err := os.ReadFile(filepath.Clean(cliPrivateKeyFixture))
	s.Require().NoError(err)
	s.writeSecret(filepath.Join(dir, "enablebanking.pem"), string(key))

	statePath := filepath.Join(dir, "state.json")
	s.Require().NoError(state.Save(statePath, &state.State{
		Version: state.CurrentVersion,
		Sessions: map[string]state.Session{
			us3Bank: {
				Provider:     "enablebanking",
				SessionID:    us3SessionID,
				ValidUntil:   time.Date(2027, 3, 20, 0, 0, 0, 0, time.UTC),
				AuthorizedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
				Accounts: []state.Account{
					{UID: us3MainUID, Hash: us3MainHash, IBAN: us3MainIBAN, Currency: "EUR", Name: us3MainName},
					{
						UID: us3OverrideUID, Hash: us3OverrideHash, IBAN: us3OverrideIBAN, Currency: "EUR",
						Name: us3OverrideName,
					},
					{
						UID: us3MultiEURUID, Hash: us3MultiEURHash, IBAN: us3MultiIBAN, Currency: "EUR",
						Name: us3MultiEURName,
					},
					{
						UID: us3MultiUSDUID, Hash: us3MultiUSDHash, IBAN: us3MultiIBAN, Currency: "USD",
						Name: us3MultiUSDName,
					},
					{
						UID: us3AmbiguousUID, Hash: us3AmbiguousHash, IBAN: us3AmbiguousIBAN, Currency: "GBP",
						Name: us3AmbiguousName,
					},
					{
						UID: us3ExcludedUID, Hash: us3ExcludedHash, IBAN: us3ExcludedIBAN, Currency: "EUR",
						Name: us3ExcludedName,
					},
					{
						UID: us3UnmappedUID, Hash: us3UnmappedHash, IBAN: us3UnmappedIBAN, Currency: "EUR",
						Name: us3UnmappedName,
					},
				},
			},
		},
	}))

	configPath := filepath.Join(dir, "config.yaml")
	s.writeSecret(configPath, s.configYAML(dir, ff.srv.URL))

	events := &eventLog{}
	provider := &fakeProvider{
		events: events,
		txs: map[string][]bank.Transaction{
			us3MultiEURUID: multiEURTxs,
			us3MultiUSDUID: multiUSDTxs,
		},
	}

	return &us3Harness{
		configPath: configPath,
		statePath:  statePath,
		env:        map[string]string{envFireflyToken: us3FireflyToken},
		firefly:    ff,
		factory:    &cliFactory{provider: provider, notifiers: map[string]*namedNotifier{}},
		stdout:     &bytes.Buffer{},
		stderr:     &bytes.Buffer{},
		clock:      time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC),
	}
}

// configYAML renders us3Bank's config: one bank, no notify: channel (every check in this suite
// runs --stdout) and the two accounts: overrides (T069's scenario 4 hash redirect and scenario 5
// exclude rule).
func (s *AcceptanceUS3Suite) configYAML(dir, fireflyURL string) string {
	var b strings.Builder

	b.WriteString("timezone: UTC\n")
	b.WriteString("window_days: 30\n")
	b.WriteString("date_tolerance_days: 3\n")
	b.WriteString("consent_warn_days: 7\n")
	b.WriteString("state_file: " + filepath.Join(dir, "state.json") + "\n")
	b.WriteString("log_file: " + filepath.Join(dir, "firefly-jar.log") + "\n")
	b.WriteString("log_level: info\n")
	b.WriteString("firefly:\n")
	b.WriteString("  url: " + fireflyURL + "\n")
	b.WriteString("enablebanking:\n")
	b.WriteString("  app_id: 00000000-0000-0000-0000-000000000000\n")
	b.WriteString("  private_key_file: " + filepath.Join(dir, "enablebanking.pem") + "\n")
	b.WriteString("  redirect_url: https://example.com/eb-callback\n")
	b.WriteString("banks:\n")
	fmt.Fprintf(
		&b,
		"  %s: { name: %q, country: %s, display: %q }\n",
		us3Bank,
		us3BankName,
		us3BankCountry,
		us3BankDisplay,
	)
	b.WriteString("accounts:\n")
	fmt.Fprintf(&b, "  - { bank: %s, hash: %q, firefly_account_id: %q }\n", us3Bank, us3OverrideHash, us3OverrideFFID)
	fmt.Fprintf(&b, "  - { bank: %s, iban: %s, exclude: true }\n", us3Bank, us3ExcludedIBAN)

	return b.String()
}

// newIndepHarness builds the Independent Test's own isolated fixture: one bank, us3IndepBank, with
// exactly two accounts and no accounts: override yet.
func (s *AcceptanceUS3Suite) newIndepHarness() *us3Harness {
	s.T().Helper()

	ffAccounts := []us3FFAccount{
		{id: us3IndepAutoFFID, name: us3IndepAutoFFName, iban: us3IndepAutoIBAN, currency: "EUR"},
		{id: us3IndepCardFFID, name: us3IndepCardFFName, iban: "", currency: "EUR"},
	}

	body, err := json.Marshal(us3AccountsPage(ffAccounts))
	s.Require().NoError(err)

	ff := &us3Firefly{accountsBody: body, entries: map[string][]us3Entry{}}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/accounts", ff.serveAccounts)
	mux.HandleFunc("GET /api/v1/accounts/{id}/transactions", ff.serveTransactions)

	ff.srv = httptest.NewServer(mux)
	s.T().Cleanup(ff.srv.Close)

	dir := s.T().TempDir()

	key, err := os.ReadFile(filepath.Clean(cliPrivateKeyFixture))
	s.Require().NoError(err)
	s.writeSecret(filepath.Join(dir, "enablebanking.pem"), string(key))

	statePath := filepath.Join(dir, "state.json")
	s.Require().NoError(state.Save(statePath, &state.State{
		Version: state.CurrentVersion,
		Sessions: map[string]state.Session{
			us3IndepBank: {
				Provider:     "enablebanking",
				SessionID:    us3IndepSessionID,
				ValidUntil:   time.Date(2027, 3, 20, 0, 0, 0, 0, time.UTC),
				AuthorizedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
				Accounts: []state.Account{
					{
						UID: us3IndepAutoUID, Hash: us3IndepAutoHash, IBAN: us3IndepAutoIBAN, Currency: "EUR",
						Name: us3IndepAutoName,
					},
					{UID: us3IndepCardUID, Hash: us3IndepCardHash, IBAN: "", Currency: "EUR", Name: us3IndepCardName},
				},
			},
		},
	}))

	configPath := filepath.Join(dir, "config.yaml")
	s.writeSecret(configPath, s.indepConfigYAML(dir, ff.srv.URL, false))

	events := &eventLog{}
	provider := &fakeProvider{events: events, txs: map[string][]bank.Transaction{}}

	return &us3Harness{
		configPath: configPath,
		statePath:  statePath,
		env:        map[string]string{envFireflyToken: us3FireflyToken},
		firefly:    ff,
		factory:    &cliFactory{provider: provider, notifiers: map[string]*namedNotifier{}},
		stdout:     &bytes.Buffer{},
		stderr:     &bytes.Buffer{},
		clock:      time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC),
	}
}

// indepConfigYAML renders the Independent Test's config: one bank, no notify: channel, and,
// once withOverride is set, the one hash override that maps the card account to Firefly III
// account #202.
func (s *AcceptanceUS3Suite) indepConfigYAML(dir, fireflyURL string, withOverride bool) string {
	var b strings.Builder

	b.WriteString("timezone: UTC\n")
	b.WriteString("window_days: 30\n")
	b.WriteString("date_tolerance_days: 3\n")
	b.WriteString("consent_warn_days: 7\n")
	b.WriteString("state_file: " + filepath.Join(dir, "state.json") + "\n")
	b.WriteString("log_file: " + filepath.Join(dir, "firefly-jar.log") + "\n")
	b.WriteString("log_level: info\n")
	b.WriteString("firefly:\n")
	b.WriteString("  url: " + fireflyURL + "\n")
	b.WriteString("enablebanking:\n")
	b.WriteString("  app_id: 00000000-0000-0000-0000-000000000000\n")
	b.WriteString("  private_key_file: " + filepath.Join(dir, "enablebanking.pem") + "\n")
	b.WriteString("  redirect_url: https://example.com/eb-callback\n")
	b.WriteString("banks:\n")
	fmt.Fprintf(&b, "  %s: { name: %q, country: LT, display: %q }\n", us3IndepBank, us3IndepBankName, us3IndepBankName)

	if withOverride {
		b.WriteString("accounts:\n")
		fmt.Fprintf(&b, "  - { bank: %s, hash: %q, firefly_account_id: %q }\n",
			us3IndepBank, us3IndepCardHash, us3IndepCardFFID)
	}

	return b.String()
}

// writeSecret writes content to path readable by the owner only.
func (s *AcceptanceUS3Suite) writeSecret(path, content string) {
	s.T().Helper()

	s.Require().NoError(os.WriteFile(path, []byte(content), cliSecretFileMode))
}

// bankTx builds one booked bank transaction in the given currency.
func (s *AcceptanceUS3Suite) bankTx(date, amount, currency, ref, description string) bank.Transaction {
	s.T().Helper()

	value, err := domain.ParseAmount(amount, currency)
	s.Require().NoError(err)

	d, err := civil.ParseDate(date)
	s.Require().NoError(err)

	return bank.Transaction{
		Date:        d,
		Amount:      value,
		Status:      bank.Booked,
		EntryRef:    ref,
		Description: description,
	}
}
