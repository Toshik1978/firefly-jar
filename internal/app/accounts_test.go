package app_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/stretchr/testify/suite"

	"github.com/Toshik1978/firefly-jar/internal/app"
	"github.com/Toshik1978/firefly-jar/internal/bank"
	"github.com/Toshik1978/firefly-jar/internal/redact"
	"github.com/Toshik1978/firefly-jar/internal/state"
)

// Anonymized identifiers of the three configured banks and their six accounts, covering every
// accountmap.Status the accounts command can report (contracts/cli.md, spec.md US3). bank1 holds an
// auto-mapped IBAN account and a hash-mapped override (a card with no IBAN, the credit-card case
// contracts/cli.md calls out); bank2 holds one IBAN shared by three currencies, mirroring spec.md
// US3 scenario 2, resolving to auto (EUR), ambiguous (USD, two Firefly candidates) and unmapped
// (GBP); bank3 holds one excluded account.
const (
	acctBank1        = "bank1"
	acctBank1Name    = "Bank One"
	acctBank1Country = "LT"
	acctBank2        = "bank2"
	acctBank2Name    = "Bank Two"
	acctBank2Country = "LT"
	acctBank3        = "bank3"
	acctBank3Name    = "Bank Three"
	acctBank3Country = "LT"

	// acctBank4 is a fourth configured bank carrying no accounts: override, used only by the two
	// session-problem cases (fix round 1 on fj-xwu.5.4): either it has no saved session at all, or a
	// saved session that recorded zero accounts.
	acctBank4        = "bank4"
	acctBank4Name    = "Bank Four"
	acctBank4Country = "LT"

	acctSessionID1 = "00000000-0000-0000-0000-000000000201"
	acctSessionID2 = "00000000-0000-0000-0000-000000000202"
	acctSessionID3 = "00000000-0000-0000-0000-000000000203"
	acctSessionID4 = "00000000-0000-0000-0000-000000000204"

	// acctMain is bank1's auto-mapped account: its IBAN and currency match exactly one Firefly III
	// asset account.
	acctMainUID         = "00000000-0000-0000-0000-000000000301"
	acctMainHash        = "hash-main-0000000000000000000000000000"
	acctMainIBAN        = "LT120000000000003456"
	acctMainName        = "Main"
	acctMainCurrency    = "EUR"
	acctMainFireflyID   = "12"
	acctMainFireflyName = "Household EUR"

	// acctCard is bank1's override-mapped account: it carries no IBAN (a card), so it is identified
	// by hash alone and mapped by an accounts: rule.
	acctCardUID         = "00000000-0000-0000-0000-000000000302"
	acctCardHash        = "hash-card-0000000000000000000000000000"
	acctCardName        = "Card"
	acctCardCurrency    = "EUR"
	acctCardFireflyID   = "43"
	acctCardFireflyName = "Credit card"

	// acctRevIBAN is the one IBAN bank2 reports under three currencies (spec.md US3 scenario 2).
	acctRevIBAN = "LT990000000000000001"

	// acctRevEUR resolves automatically: the only Firefly III EUR account under acctRevIBAN.
	acctRevEURUID         = "00000000-0000-0000-0000-000000000303"
	acctRevEURHash        = "hash-reveur-00000000000000000000000000"
	acctRevEURName        = "Revolut EUR"
	acctRevEURCurrency    = "EUR"
	acctRevEURFireflyID   = "20"
	acctRevEURFireflyName = "Revolut EUR"

	// acctRevUSD resolves as ambiguous: two Firefly III USD accounts share acctRevIBAN.
	acctRevUSDUID          = "00000000-0000-0000-0000-000000000304"
	acctRevUSDHash         = "hash-revusd-00000000000000000000000000"
	acctRevUSDName         = "Revolut USD"
	acctRevUSDCurrency     = "USD"
	acctRevUSDFireflyIDA   = "21"
	acctRevUSDFireflyIDB   = "22"
	acctRevUSDFireflyNameA = "Revolut USD A"
	acctRevUSDFireflyNameB = "Revolut USD B"

	// acctRevGBP resolves as unmapped: no Firefly III account shares acctRevIBAN in GBP.
	acctRevGBPUID      = "00000000-0000-0000-0000-000000000305"
	acctRevGBPHash     = "hash-revgbp-00000000000000000000000000"
	acctRevGBPName     = "Revolut GBP"
	acctRevGBPCurrency = "GBP"

	// acctClosed is bank3's excluded account.
	acctClosedUID      = "00000000-0000-0000-0000-000000000306"
	acctClosedHash     = "hash-closed-00000000000000000000000000"
	acctClosedIBAN     = "LT550000000000007777"
	acctClosedName     = "Closed"
	acctClosedCurrency = "EUR"

	acctFireflyToken = "test-firefly-token-0001"

	// acctHeader is the accounts table header, without --ids (contracts/cli.md).
	acctHeader = "BANK ACCOUNT NAME CUR STATUS FIREFLY"
	// acctHeaderWithIDs is the header --ids adds, with a trailing HASH column.
	acctHeaderWithIDs = acctHeader + " HASH"
)

// acctColumnSplit splits one rendered table line into its cell values on runs of two or more
// spaces, tolerating whatever exact tabwriter padding the implementation chooses without
// hardcoding column widths, while still splitting single-space-padded rows apart from single
// spaces inside a cell's own text (e.g. "Revolut EUR", "#21, #22").
var acctColumnSplit = regexp.MustCompile(`\s{2,}`)

// splitColumns parses one line of the accounts table into its cell values.
func splitColumns(line string) []string {
	return acctColumnSplit.Split(strings.TrimRight(line, " "), -1)
}

// acctFFAccount is one Firefly III asset account the fake serves from GET /accounts.
type acctFFAccount struct {
	id       string
	name     string
	iban     string
	currency string
}

// acctAccountsPage renders accounts as one Firefly III GET /accounts page (no meta block, as
// testdata/firefly/accounts_nometa.json does for a single page).
func acctAccountsPage(accounts []acctFFAccount) map[string]any {
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

// acctMethodLog records the HTTP method of every request the fake Firefly III server received, so
// a case can prove the accounts command never sends anything but GET (contracts/cli.md, constitution
// §I).
type acctMethodLog struct {
	mu      sync.Mutex
	methods []string
}

// wrap returns h wrapped to record every request's method before serving it.
func (l *acctMethodLog) wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		l.methods = append(l.methods, r.Method)
		l.mu.Unlock()

		h.ServeHTTP(w, r)
	})
}

// all returns a copy of every method recorded so far.
func (l *acctMethodLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return append([]string(nil), l.methods...)
}

// acctHarness is one accounts-command case: a temp dir holding the config and state, the fake
// Firefly III (with its method log) the config points at, the fake bank provider through a
// cliFactory, and the three console streams. It plays the same role cliHarness plays for CLISuite,
// kept separate here so this suite can add its own request-method recording without changing the
// shared harness other suites use.
type acctHarness struct {
	configPath string
	statePath  string
	env        map[string]string
	firefly    *fakeFirefly
	factory    *cliFactory
	methods    *acctMethodLog
	stdout     *bytes.Buffer
	stderr     *bytes.Buffer
	clock      time.Time
}

// run invokes app.RunEnv with args and the harness's Env.
func (h *acctHarness) run(args ...string) int {
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

// AccountsCommandSuite covers the read-only `accounts [--ids]` command (T067, FR-017,
// contracts/cli.md, spec.md US3). It is the RED half of T068: internal/app/accounts.go does not
// exist yet, and the accounts command is a stub that always prints "not implemented" to stderr and
// exits 2 without reading config, state or Firefly III.
//
// Every case builds its own config and state in a fresh temp dir and points firefly.url at an
// httptest Firefly III serving a fixed accounts fixture; the fake bank provider records every call
// it receives, so a case can prove accounts makes none.
type AccountsCommandSuite struct {
	suite.Suite
}

// TestTableColumnsAndStatuses covers the accounts table's shape (contracts/cli.md): the exact
// header, one row per bank account in bank-then-account order, the masked ACCOUNT (IBAN or hash),
// every accountmap.Status this build can produce, and the FIREFLY column's three renderings (mapped,
// ambiguous candidates, and "—" for anything else). The fixture's ambiguous and unmapped accounts
// make this run exit 2.
func (s *AccountsCommandSuite) TestTableColumnsAndStatuses() {
	h := s.newHarness(true)

	code := h.run("accounts", "--config", h.configPath)

	s.Equal(2, code, "an ambiguous or unmapped non-excluded account fails the setup check")
	s.Empty(h.stderr.String(), "a mapping-only exit 2 writes nothing to stderr")

	lines := s.outputLines(h)
	s.Require().Len(lines, 7, "one header line plus one row per of the fixture's six accounts")
	s.Equal(strings.Fields(acctHeader), splitColumns(lines[0]))

	want := [][]string{
		{
			acctBank1, redact.MaskIBAN(acctMainIBAN), acctMainName, acctMainCurrency, "auto",
			"#" + acctMainFireflyID + " " + acctMainFireflyName,
		},
		{
			acctBank1, redact.MaskHash(acctCardHash), acctCardName, acctCardCurrency, "override",
			"#" + acctCardFireflyID + " " + acctCardFireflyName,
		},
		{
			acctBank2, redact.MaskIBAN(acctRevIBAN), acctRevEURName, acctRevEURCurrency, "auto",
			"#" + acctRevEURFireflyID + " " + acctRevEURFireflyName,
		},
		{
			acctBank2, redact.MaskIBAN(acctRevIBAN), acctRevUSDName, acctRevUSDCurrency, "ambiguous",
			"#" + acctRevUSDFireflyIDA + ", #" + acctRevUSDFireflyIDB,
		},
		{acctBank2, redact.MaskIBAN(acctRevIBAN), acctRevGBPName, acctRevGBPCurrency, "unmapped", "—"},
		{acctBank3, redact.MaskIBAN(acctClosedIBAN), acctClosedName, acctClosedCurrency, "excluded", "—"},
	}

	for i, row := range want {
		s.Equal(row, splitColumns(lines[i+1]), "row %d", i)
	}
}

// TestIDsFlagAddsHashColumn covers --ids (contracts/cli.md): the header gains a trailing HASH
// column, and every row's HASH is the account's full, unmasked identification_hash, for
// copy-paste into an accounts: override.
func (s *AccountsCommandSuite) TestIDsFlagAddsHashColumn() {
	h := s.newHarness(true)

	code := h.run("accounts", "--ids", "--config", h.configPath)

	s.Equal(2, code)

	lines := s.outputLines(h)
	s.Require().Len(lines, 7)
	s.Equal(strings.Fields(acctHeaderWithIDs), splitColumns(lines[0]))

	wantHashes := []string{acctMainHash, acctCardHash, acctRevEURHash, acctRevUSDHash, acctRevGBPHash, acctClosedHash}
	for i, hash := range wantHashes {
		cols := splitColumns(lines[i+1])
		s.Require().Len(cols, 7, "row %d gains a trailing HASH column", i)
		s.Equal(hash, cols[6], "row %d HASH is the full, unmasked hash", i)
	}
}

// TestExitCodeZeroWhenEveryNonExcludedAccountIsMapped covers the other side of contracts/cli.md's
// exit rule: with the ambiguous and unmapped accounts removed, every remaining non-excluded account
// is auto or override mapped, so the run exits 0.
func (s *AccountsCommandSuite) TestExitCodeZeroWhenEveryNonExcludedAccountIsMapped() {
	h := s.newHarness(false)

	code := h.run("accounts", "--config", h.configPath)

	s.Equal(0, code)
	s.Empty(h.stderr.String())

	lines := s.outputLines(h)
	s.Require().Len(lines, 5, "one header line plus main, card, bank2's auto-mapped EUR account and closed")
}

// TestBankSuppliedTextIsStrippedBeforePrinting covers the terminal side of the digest's rule: an
// account name the bank supplied can carry a line break, a tab or a bidi override, which would
// add a fake row, shift the table's columns or reorder what the owner reads. The accounts table
// prints it only after the same strip the digest applies, so the row stays one row of six cells.
func (s *AccountsCommandSuite) TestBankSuppliedTextIsStrippedBeforePrinting() {
	h := s.newHarness(false)

	st, _, err := state.Load(h.statePath)
	s.Require().NoError(err)

	session := st.Sessions[acctBank1]
	session.Accounts[0].Name = "Ma\u202ein\nFAKE\tROW"
	st.Put(acctBank1, session)
	s.Require().NoError(state.Save(h.statePath, st))

	code := h.run("accounts", "--config", h.configPath)

	s.Equal(0, code)

	lines := s.outputLines(h)
	s.Require().Len(lines, 5, "a newline in a bank-supplied name must not add a row")
	s.NotContains(h.stdout.String(), "\u202e", "a bidi override must never reach the terminal")

	s.Equal([]string{
		acctBank1, redact.MaskIBAN(acctMainIBAN), "MainFAKEROW", acctMainCurrency, "auto",
		"#" + acctMainFireflyID + " " + acctMainFireflyName,
	}, splitColumns(lines[1]), "a tab in a bank-supplied name must not add a column")
}

// TestReadOnly covers contracts/cli.md's read-only guarantee (FR-017, constitution §I): accounts
// makes no bank-provider call because it reads accounts from the state snapshot, the Firefly III
// fake sees only GET requests, and the state file itself is left byte-for-byte and mtime-for-mtime
// unchanged.
func (s *AccountsCommandSuite) TestReadOnly() {
	h := s.newHarness(true)

	before, err := os.ReadFile(filepath.Clean(h.statePath))
	s.Require().NoError(err)
	beforeInfo, err := os.Stat(h.statePath)
	s.Require().NoError(err)

	code := h.run("accounts", "--config", h.configPath)
	s.Require().Equal(2, code)

	after, err := os.ReadFile(filepath.Clean(h.statePath))
	s.Require().NoError(err)
	afterInfo, err := os.Stat(h.statePath)
	s.Require().NoError(err)

	s.Equal(before, after, "accounts never rewrites the state file")
	s.Equal(beforeInfo.ModTime(), afterInfo.ModTime(), "accounts never touches the state file")

	s.Empty(h.factory.provider.recorded(), "accounts makes no bank-provider call")
	s.Empty(h.firefly.recorded(), "accounts fetches no Firefly III transactions")

	methods := h.methods.all()
	s.NotEmpty(methods, "accounts does call Firefly III to list accounts")

	for _, m := range methods {
		s.Equal(http.MethodGet, m, "every Firefly III request is a GET")
	}
}

// TestBankWithoutSessionReportsProblemAndExitsNonZero covers a configured bank that has never been
// authorized: accounts must not silently drop it from the setup check (fix round 1 on fj-xwu.5.4).
// It still prints every other bank's rows, but reports exactly one stderr line naming the bank and
// the fix, and exits 2, the same as check.go's usableSession would refuse to check it.
func (s *AccountsCommandSuite) TestBankWithoutSessionReportsProblemAndExitsNonZero() {
	h := s.newHarnessWithBank4(false)

	code := h.run("accounts", "--config", h.configPath)

	s.Equal(2, code)
	s.Equal(
		"firefly-jar: "+acctBank4+": not authorized — run: firefly-jar auth "+acctBank4+"\n",
		h.stderr.String(),
	)

	lines := s.outputLines(h)
	s.Require().Len(lines, 5, "one header line plus main, card, bank2's auto EUR account and closed")
}

// TestBankWithEmptySessionReportsProblemAndExitsNonZero covers a saved session that recorded zero
// accounts: the same problem family as no session at all, with the same fix hint (fix round 1 on
// fj-xwu.5.4).
func (s *AccountsCommandSuite) TestBankWithEmptySessionReportsProblemAndExitsNonZero() {
	h := s.newHarnessWithBank4(true)

	code := h.run("accounts", "--config", h.configPath)

	s.Equal(2, code)
	s.Equal(
		"firefly-jar: "+acctBank4+": no accounts in the saved session — run: firefly-jar auth "+acctBank4+"\n",
		h.stderr.String(),
	)

	lines := s.outputLines(h)
	s.Require().Len(lines, 5, "one header line plus main, card, bank2's auto EUR account and closed")
}

// outputLines splits h's stdout into its non-empty lines, requiring the run wrote a table there
// (the RED stub instead writes its error to stderr and nothing to stdout).
func (s *AccountsCommandSuite) outputLines(h *acctHarness) []string {
	s.T().Helper()

	out := strings.TrimRight(h.stdout.String(), "\n")
	s.Require().NotEmpty(out, "accounts prints its table to stdout")

	return strings.Split(out, "\n")
}

// newHarness builds a fresh config, state and fake Firefly III for one case. withAmbiguity
// includes bank2's ambiguous (USD) and unmapped (GBP) accounts; without it, every non-excluded
// account is mapped, so a case can exercise the exit 0 side of contracts/cli.md.
func (s *AccountsCommandSuite) newHarness(withAmbiguity bool) *acctHarness {
	s.T().Helper()

	ff := []acctFFAccount{
		{id: acctMainFireflyID, name: acctMainFireflyName, iban: acctMainIBAN, currency: acctMainCurrency},
		{id: acctCardFireflyID, name: acctCardFireflyName, iban: "", currency: acctCardCurrency},
		{id: acctRevEURFireflyID, name: acctRevEURFireflyName, iban: acctRevIBAN, currency: acctRevEURCurrency},
	}

	bank2Accounts := []state.Account{
		{
			UID:      acctRevEURUID,
			Hash:     acctRevEURHash,
			IBAN:     acctRevIBAN,
			Currency: acctRevEURCurrency,
			Name:     acctRevEURName,
		},
	}

	if withAmbiguity {
		ff = append(
			ff,
			acctFFAccount{
				id:       acctRevUSDFireflyIDA,
				name:     acctRevUSDFireflyNameA,
				iban:     acctRevIBAN,
				currency: acctRevUSDCurrency,
			},
			acctFFAccount{
				id:       acctRevUSDFireflyIDB,
				name:     acctRevUSDFireflyNameB,
				iban:     acctRevIBAN,
				currency: acctRevUSDCurrency,
			},
		)
		bank2Accounts = append(
			bank2Accounts,
			state.Account{
				UID:      acctRevUSDUID,
				Hash:     acctRevUSDHash,
				IBAN:     acctRevIBAN,
				Currency: acctRevUSDCurrency,
				Name:     acctRevUSDName,
			},
			state.Account{
				UID:      acctRevGBPUID,
				Hash:     acctRevGBPHash,
				IBAN:     acctRevIBAN,
				Currency: acctRevGBPCurrency,
				Name:     acctRevGBPName,
			},
		)
	}

	events := &eventLog{}
	methods := &acctMethodLog{}
	fireflyFake := &fakeFirefly{events: events}

	body, err := json.Marshal(acctAccountsPage(ff))
	s.Require().NoError(err)
	fireflyFake.accountsBody = body

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/accounts", fireflyFake.serveAccounts)
	mux.HandleFunc("GET /api/v1/accounts/{id}/transactions", fireflyFake.serveTransactions)

	fireflyFake.srv = httptest.NewServer(methods.wrap(mux))
	s.T().Cleanup(fireflyFake.srv.Close)

	dir := s.T().TempDir()

	key, err := os.ReadFile(filepath.Clean(cliPrivateKeyFixture))
	s.Require().NoError(err)
	s.Require().NoError(os.WriteFile(filepath.Join(dir, "enablebanking.pem"), key, cliSecretFileMode))

	statePath := filepath.Join(dir, "state.json")
	s.writeState(statePath, bank2Accounts, nil)

	configPath := filepath.Join(dir, "config.yaml")
	s.Require().NoError(
		os.WriteFile(configPath, []byte(s.configYAML(dir, fireflyFake.srv.URL, false)), cliSecretFileMode),
	)

	return &acctHarness{
		configPath: configPath,
		statePath:  statePath,
		env:        map[string]string{envFireflyToken: acctFireflyToken},
		firefly:    fireflyFake,
		factory: &cliFactory{
			provider:  &fakeProvider{events: events, txs: map[string][]bank.Transaction{}},
			notifiers: map[string]*namedNotifier{},
		},
		stdout:  &bytes.Buffer{},
		stderr:  &bytes.Buffer{},
		clock:   time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC),
		methods: methods,
	}
}

// newHarnessWithBank4 builds the same complete-mapping fixture as newHarness(false) (main, card,
// bank2's auto-mapped EUR account and closed, every non-excluded account auto or override mapped),
// plus a fourth configured bank, bank4, that carries no accounts: override. When hasSession is
// false, bank4 has no saved session at all; when true, it has a saved session that recorded zero
// accounts. Both are session-problem cases the accounts command must report rather than silently
// drop (fix round 1 on fj-xwu.5.4).
func (s *AccountsCommandSuite) newHarnessWithBank4(hasSession bool) *acctHarness {
	s.T().Helper()

	ff := []acctFFAccount{
		{id: acctMainFireflyID, name: acctMainFireflyName, iban: acctMainIBAN, currency: acctMainCurrency},
		{id: acctCardFireflyID, name: acctCardFireflyName, iban: "", currency: acctCardCurrency},
		{id: acctRevEURFireflyID, name: acctRevEURFireflyName, iban: acctRevIBAN, currency: acctRevEURCurrency},
	}

	bank2Accounts := []state.Account{
		{
			UID:      acctRevEURUID,
			Hash:     acctRevEURHash,
			IBAN:     acctRevIBAN,
			Currency: acctRevEURCurrency,
			Name:     acctRevEURName,
		},
	}

	var extra map[string]state.Session
	if hasSession {
		extra = map[string]state.Session{
			acctBank4: {
				Provider:     "enablebanking",
				SessionID:    acctSessionID4,
				ValidUntil:   time.Date(2027, 3, 20, 0, 0, 0, 0, time.UTC),
				AuthorizedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
				Accounts:     []state.Account{},
			},
		}
	}

	events := &eventLog{}
	methods := &acctMethodLog{}
	fireflyFake := &fakeFirefly{events: events}

	body, err := json.Marshal(acctAccountsPage(ff))
	s.Require().NoError(err)
	fireflyFake.accountsBody = body

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/accounts", fireflyFake.serveAccounts)
	mux.HandleFunc("GET /api/v1/accounts/{id}/transactions", fireflyFake.serveTransactions)

	fireflyFake.srv = httptest.NewServer(methods.wrap(mux))
	s.T().Cleanup(fireflyFake.srv.Close)

	dir := s.T().TempDir()

	key, err := os.ReadFile(filepath.Clean(cliPrivateKeyFixture))
	s.Require().NoError(err)
	s.Require().NoError(os.WriteFile(filepath.Join(dir, "enablebanking.pem"), key, cliSecretFileMode))

	statePath := filepath.Join(dir, "state.json")
	s.writeState(statePath, bank2Accounts, extra)

	configPath := filepath.Join(dir, "config.yaml")
	s.Require().NoError(
		os.WriteFile(configPath, []byte(s.configYAML(dir, fireflyFake.srv.URL, true)), cliSecretFileMode),
	)

	return &acctHarness{
		configPath: configPath,
		statePath:  statePath,
		env:        map[string]string{envFireflyToken: acctFireflyToken},
		firefly:    fireflyFake,
		factory: &cliFactory{
			provider:  &fakeProvider{events: events, txs: map[string][]bank.Transaction{}},
			notifiers: map[string]*namedNotifier{},
		},
		stdout:  &bytes.Buffer{},
		stderr:  &bytes.Buffer{},
		clock:   time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC),
		methods: methods,
	}
}

// writeState saves a state file with bank1 (main and card), bank2 (bank2Accounts) and bank3
// (closed), plus any extra sessions (used only by the bank-with-a-session-problem cases, keyed by
// bank so a case can add or omit bank4's own session).
func (s *AccountsCommandSuite) writeState(path string, bank2Accounts []state.Account, extra map[string]state.Session) {
	s.T().Helper()

	sessions := map[string]state.Session{
		acctBank1: {
			Provider:     "enablebanking",
			SessionID:    acctSessionID1,
			ValidUntil:   time.Date(2027, 3, 20, 0, 0, 0, 0, time.UTC),
			AuthorizedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
			Accounts: []state.Account{
				{
					UID:      acctMainUID,
					Hash:     acctMainHash,
					IBAN:     acctMainIBAN,
					Currency: acctMainCurrency,
					Name:     acctMainName,
				},
				{UID: acctCardUID, Hash: acctCardHash, IBAN: "", Currency: acctCardCurrency, Name: acctCardName},
			},
		},
		acctBank2: {
			Provider:     "enablebanking",
			SessionID:    acctSessionID2,
			ValidUntil:   time.Date(2027, 3, 20, 0, 0, 0, 0, time.UTC),
			AuthorizedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
			Accounts:     bank2Accounts,
		},
		acctBank3: {
			Provider:     "enablebanking",
			SessionID:    acctSessionID3,
			ValidUntil:   time.Date(2027, 3, 20, 0, 0, 0, 0, time.UTC),
			AuthorizedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
			Accounts: []state.Account{
				{
					UID: acctClosedUID, Hash: acctClosedHash, IBAN: acctClosedIBAN,
					Currency: acctClosedCurrency, Name: acctClosedName,
				},
			},
		},
	}

	maps.Copy(sessions, extra)

	s.Require().NoError(state.Save(path, &state.State{Version: state.CurrentVersion, Sessions: sessions}))
}

// configYAML renders the case's config: three banks, no notify: channel (accounts builds none) and
// one accounts: override per non-automatic mapping (bank1's card, by hash, and bank3's closed
// account, excluded). withBank4 also declares acctBank4 under banks:, with no accounts: override of
// its own, for the two session-problem cases (fix round 1 on fj-xwu.5.4).
func (s *AccountsCommandSuite) configYAML(dir, fireflyURL string, withBank4 bool) string {
	banks := []configBank{
		{key: acctBank1, name: acctBank1Name, country: acctBank1Country},
		{key: acctBank2, name: acctBank2Name, country: acctBank2Country},
		{key: acctBank3, name: acctBank3Name, country: acctBank3Country},
	}

	if withBank4 {
		banks = append(banks, configBank{key: acctBank4, name: acctBank4Name, country: acctBank4Country})
	}

	return buildConfigYAML(configOpts{
		timezone:       "UTC",
		stateFile:      filepath.Join(dir, "state.json"),
		logFile:        filepath.Join(dir, "firefly-jar.log"),
		fireflyURL:     fireflyURL,
		privateKeyFile: filepath.Join(dir, "enablebanking.pem"),
		banks:          banks,
		overrides: []string{
			fmt.Sprintf(
				"  - { bank: %s, hash: %q, firefly_account_id: %q }\n", acctBank1, acctCardHash, acctCardFireflyID,
			),
			fmt.Sprintf("  - { bank: %s, iban: %s, exclude: true }\n", acctBank3, acctClosedIBAN),
		},
	})
}
