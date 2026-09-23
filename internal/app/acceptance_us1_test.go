package app_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/stretchr/testify/suite"

	"github.com/Toshik1978/firefly-jar/internal/bank"
	"github.com/Toshik1978/firefly-jar/internal/civil"
	"github.com/Toshik1978/firefly-jar/internal/domain"
	"github.com/Toshik1978/firefly-jar/internal/state"
)

// The two transactions the Independent Test drives (spec.md User Story 1): one already entered in
// Firefly III within tolerance, one not entered at all. acceptanceMissingLine is the digest line
// digest.Render gives the unentered one; acceptanceEnteredDescription is the entered one's own
// description, which must never appear in a digest that reports only the other as missing.
const (
	acceptanceMissingLine        = "- 2026-09-19  -63.12 EUR  ANON GROCERY"
	acceptanceEnteredDescription = "ANON BAKERY"
)

// AcceptanceUS1Suite is the US1 Independent Test (T056, spec.md User Story 1, tasks.md Phase 3):
// it drives the real CLI path app.RunEnv -> check end to end, with a hand-provisioned state.json
// holding one session and one auto-mapped Firefly III account. A fake bank.Provider returns two
// bank transactions; the fake Firefly III has only one of them entered. The first run must exit 1
// and deliver exactly one digest naming exactly the unentered transaction. Once the fixture is
// updated to include it, the next run must exit 0 and send nothing. A third run with --stdout must
// print the same digest instead of calling any notifier.
type AcceptanceUS1Suite struct {
	suite.Suite

	vilnius *time.Location
}

// SetupSuite loads the configured time zone once.
func (s *AcceptanceUS1Suite) SetupSuite() {
	loc, err := time.LoadLocation("Europe/Vilnius")
	s.Require().NoError(err)

	s.vilnius = loc
}

// TestOneMissingTransactionIsRemindedThenClearedOnceEntered covers the Independent Test's sending
// path: a check with one unentered bank transaction exits 1 and delivers exactly one digest naming
// only that transaction; entering it in Firefly III and running again exits 0 and sends nothing.
func (s *AcceptanceUS1Suite) TestOneMissingTransactionIsRemindedThenClearedOnceEntered() {
	h := s.newHarness(
		[]bank.Transaction{
			s.bankTx("2026-09-20", "-12.40", "ref-1", acceptanceEnteredDescription),
			s.bankTx("2026-09-19", "-63.12", "ref-2", "ANON GROCERY"),
		},
		[]ffEntry{
			{group: "501", kind: "withdrawal", date: "2026-09-21", amount: "12.40", description: "FF BAKERY"},
		},
	)

	code := h.run("check", "--config", h.configPath)

	s.Equal(1, code, "one unentered transaction gives exit 1")
	s.Empty(h.stdout.String(), "a delivered run writes nothing to stdout")
	s.Empty(h.stderr.String(), "a delivered run writes nothing to stderr")

	first := h.factory.notifier("telegram")
	s.Require().NotNil(first, "the configured telegram notifier is built")
	s.Require().Len(first.sent(), 1, "exactly one digest is delivered")

	text := first.sent()[0].Text()
	s.Contains(text, acceptanceMissingLine, "the digest names exactly the unentered transaction")
	s.NotContains(text, acceptanceEnteredDescription, "the already-entered transaction is never listed")

	// The owner enters the missing transaction in Firefly III.
	h.firefly.entries = append(h.firefly.entries, ffEntry{
		group: "502", kind: "withdrawal", date: "2026-09-19", amount: "63.12", description: "FF GROCERY",
	})

	code = h.run("check", "--config", h.configPath)

	s.Equal(0, code, "everything is now entered")
	s.Empty(h.stdout.String())
	s.Empty(h.stderr.String())

	second := h.factory.notifier("telegram")
	s.Require().NotNil(second)
	s.Empty(second.sent(), "no digest is sent once everything is entered")
}

// TestStdoutPrintsTheDigestInsteadOfSendingIt covers the Independent Test run with --stdout: the
// digest reaches stdout instead of any notifier, with the same exit code a sending run would give.
func (s *AcceptanceUS1Suite) TestStdoutPrintsTheDigestInsteadOfSendingIt() {
	h := s.newHarness(
		[]bank.Transaction{
			s.bankTx("2026-09-20", "-12.40", "ref-1", acceptanceEnteredDescription),
			s.bankTx("2026-09-19", "-63.12", "ref-2", "ANON GROCERY"),
		},
		[]ffEntry{
			{group: "501", kind: "withdrawal", date: "2026-09-21", amount: "12.40", description: "FF BAKERY"},
		},
	)

	code := h.run("check", "--stdout", "--config", h.configPath)

	s.Equal(1, code)
	s.Contains(h.stdout.String(), acceptanceMissingLine)
	s.NotContains(h.stdout.String(), acceptanceEnteredDescription)
	s.Empty(h.stderr.String())

	built := h.factory.builtNames()
	s.Require().Len(built, 1)
	s.Empty(built[0], "check --stdout builds no notifier")
	s.Nil(h.factory.notifier("telegram"), "check --stdout never calls a notifier")
}

// newHarness writes a config with a single Telegram notifier, a state file with one session
// mapped to Firefly III account #1 (checkAccountsFile, as check_test.go's fixture defines it), and
// a fake Firefly III serving entries on that account. The fake provider returns bankTxs for the
// session's one account.
func (s *AcceptanceUS1Suite) newHarness(bankTxs []bank.Transaction, entries []ffEntry) *cliHarness {
	s.T().Helper()

	accounts, err := os.ReadFile(filepath.Clean(checkAccountsFile))
	s.Require().NoError(err)

	events := &eventLog{}
	ff := &fakeFirefly{events: events, accountsBody: accounts, entries: entries}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/accounts", ff.serveAccounts)
	mux.HandleFunc("GET /api/v1/accounts/{id}/transactions", ff.serveTransactions)

	ff.srv = httptest.NewServer(mux)
	s.T().Cleanup(ff.srv.Close)

	dir := s.T().TempDir()
	h := &cliHarness{
		configPath: filepath.Join(dir, "config.yaml"),
		logPath:    filepath.Join(dir, "firefly-jar.log"),
		env:        map[string]string{envFireflyToken: checkFireflyToken, envTelegramToken: cliTelegramToken},
		firefly:    ff,
		factory: &cliFactory{
			provider:  &fakeProvider{events: events, txs: map[string][]bank.Transaction{checkAccountUID: bankTxs}},
			notifiers: map[string]*namedNotifier{},
		},
		stdout: &bytes.Buffer{},
		stderr: &bytes.Buffer{},
		clock:  time.Date(2026, 9, 22, 10, 0, 0, 0, s.vilnius),
	}

	key, err := os.ReadFile(filepath.Clean(cliPrivateKeyFixture))
	s.Require().NoError(err)
	s.writeSecret(filepath.Join(dir, "enablebanking.pem"), string(key))

	s.writeState(filepath.Join(dir, "state.json"))
	s.writeSecret(h.configPath, s.configYAML(dir, ff.srv.URL))

	return h
}

// configYAML renders the case's config: one bank, one Telegram notifier, and the given Firefly III
// URL.
func (*AcceptanceUS1Suite) configYAML(dir, fireflyURL string) string {
	var b strings.Builder

	b.WriteString("timezone: Europe/Vilnius\n")
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
	b.WriteString("  " + checkBankKey + ": { name: Test Bank, country: LT, display: Testbank }\n")
	b.WriteString("notify:\n")
	b.WriteString("  telegram:\n")
	b.WriteString("    bot_token_file: " + filepath.Join(dir, "telegram.token") + "\n")
	b.WriteString("    chat_ids: [100000001]\n")

	return b.String()
}

// writeState saves a state file with one session for the configured bank whose one account maps
// automatically to Firefly III account #1.
func (s *AcceptanceUS1Suite) writeState(path string) {
	s.T().Helper()

	s.Require().NoError(state.Save(path, &state.State{
		Version: state.CurrentVersion,
		Sessions: map[string]state.Session{
			checkBankKey: {
				Provider:     "enablebanking",
				SessionID:    checkSessionID,
				ValidUntil:   time.Date(2027, 3, 20, 0, 0, 0, 0, time.UTC),
				AuthorizedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
				Accounts: []state.Account{{
					UID:      checkAccountUID,
					Hash:     checkAccountHash,
					IBAN:     checkAccountIBAN,
					Currency: "EUR",
					Name:     "Main",
				}},
			},
		},
	}))
}

// writeSecret writes content to path readable by the owner only.
func (s *AcceptanceUS1Suite) writeSecret(path, content string) {
	s.T().Helper()

	s.Require().NoError(os.WriteFile(path, []byte(content), cliSecretFileMode))
}

// bankTx builds one booked EUR bank transaction.
func (s *AcceptanceUS1Suite) bankTx(date, amount, ref, description string) bank.Transaction {
	s.T().Helper()

	value, err := domain.ParseAmount(amount, "EUR")
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
