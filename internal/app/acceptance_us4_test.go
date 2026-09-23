package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	"github.com/Toshik1978/firefly-jar/internal/money"
	"github.com/Toshik1978/firefly-jar/internal/notify"
	"github.com/Toshik1978/firefly-jar/internal/redact"
	"github.com/Toshik1978/firefly-jar/internal/state"
)

// Anonymized fixture identifiers for AcceptanceUS4Suite (T076, spec.md User Story 4). us4Bank
// carries scenario 1's single account (Firefly III itself is unreachable, so the account never
// even reaches mapping); us4BankA/us4BankB carry scenario 2's two banks, one account each, both
// auto-mapping by IBAN and currency to their own Firefly III asset account.
const (
	us4Bank        = "us4bank"
	us4BankName    = "US4 Bank"
	us4BankDisplay = "US4 Bank"
	us4SessionID   = "00000000-0000-0000-0000-000000000601"
	us4AccountUID  = "00000000-0000-0000-0000-000000000611"
	us4AccountHash = "hash-us4-000000000000000000000000000"
	us4AccountIBAN = "LT610000000000000601"
	us4AccountName = "Main"

	us4BankA        = "us4banka"
	us4BankAName    = "US4 Bank A"
	us4BankADisplay = "US4 Bank A"
	us4SessionA     = "00000000-0000-0000-0000-000000000602"
	us4AccountAUID  = "00000000-0000-0000-0000-000000000612"
	us4AccountAHash = "hash-us4a-00000000000000000000000000"
	us4AccountAIBAN = "LT610000000000000602"
	us4AccountAName = "A Main"
	us4FireflyIDA   = "610"

	us4BankB        = "us4bankb"
	us4BankBName    = "US4 Bank B"
	us4BankBDisplay = "US4 Bank B"
	us4SessionB     = "00000000-0000-0000-0000-000000000603"
	us4AccountBUID  = "00000000-0000-0000-0000-000000000613"
	us4AccountBHash = "hash-us4b-00000000000000000000000000"
	us4AccountBIBAN = "LT610000000000000603"
	us4AccountBName = "B Main"
	us4FireflyIDB   = "611"

	us4FireflyToken = "test-firefly-token-0400"
)

// us4FFAccount is one Firefly III asset account the fake accounts endpoint serves for scenario 2.
type us4FFAccount struct {
	id, iban, currency, name string
}

// us4AccountsPage renders accounts as one Firefly III GET /accounts page (no meta block), the same
// shape check_test.go's fixture and the other acceptance suites' fakes use.
func us4AccountsPage(accounts []us4FFAccount) map[string]any {
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

// us4Factory is the app.Factory AcceptanceUS4Suite's scenario 1 and 2 cases inject: it lets the
// production app.BuildDeps build everything for real (so config, secrets and state are validated
// for real, exactly like the other acceptance suites), then substitutes the scripted bank.Provider
// a case needs. Scenario 5 (invalid config) reuses failfast_test.go's own ffFactory/ffHarness/
// ffPaths instead of a copy of that pattern here.
type us4Factory struct {
	provider bank.Provider
}

// build is the app.Factory the harness injects.
func (f *us4Factory) build(ctx context.Context, in app.BuildInput) (app.Deps, error) {
	deps, err := app.BuildDeps(ctx, in)
	if err != nil {
		return app.Deps{}, fmt.Errorf("build deps: %w", err)
	}

	deps.Provider = f.provider

	return deps, nil
}

// us4Harness is one AcceptanceUS4Suite case: a temp dir holding the config, state and secret
// files, a fake Firefly III, the us4Factory, an eventLog shared with the fake Firefly III and bank
// provider (so a case can prove which external systems were, or were not, called), and the console
// buffers. It drives the real CLI path app.RunEnv, exactly like AcceptanceUS1-3Suite.
type us4Harness struct {
	configPath string
	env        map[string]string
	firefly    *fakeFirefly
	factory    *us4Factory
	events     *eventLog
	stdout     *bytes.Buffer
	stderr     *bytes.Buffer
	clock      time.Time
}

// run resets the console buffers, then invokes app.RunEnv with args and the harness's Env.
func (h *us4Harness) run(args ...string) int {
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

// AcceptanceUS4Suite is the US4 acceptance test (T076, spec.md User Story 4): it drives the real
// CLI path app.RunEnv through `check`, end to end, for each of US4's five acceptance scenarios.
// Scenarios 1 and 2 build their own fixture through us4Factory/us4Harness; scenario 5 reuses
// failfast_test.go's ffFactory/ffHarness/ffPaths (FailFastSuite's own zero-hits harness); scenarios
// 3 and 4 reuse delivery_test.go's deliveryFactory/deliveryNotifier exactly as DeliverySuite does.
// Reusing each, rather than a per-scenario copy, means a defect any of them surfaces is a defect in
// check.go, cli.go or config.go, never a test-only fake.
type AcceptanceUS4Suite struct {
	suite.Suite

	vilnius *time.Location
}

// SetupSuite loads the configured time zone once, for scenario 3 and 4's delivery fixture.
func (s *AcceptanceUS4Suite) SetupSuite() {
	loc, err := time.LoadLocation("Europe/Vilnius")
	s.Require().NoError(err)

	s.vilnius = loc
}

// TestScenario1FireflyUnreachableSendsProblemOnlyDigestAndEndsCheckFailed covers spec.md US4
// acceptance scenario 1: Firefly III unreachable (a non-retried 404, so the case costs no real
// sleep, the same technique isolation_test.go's IsolationSuite already uses for this exact case)
// yields a problem-only digest and the run ends with the "check failed" outcome (exit 2), having
// never called the bank at all even though one is configured.
func (s *AcceptanceUS4Suite) TestScenario1FireflyUnreachableSendsProblemOnlyDigestAndEndsCheckFailed() {
	h := s.newFireflyUnreachableHarness()

	code := h.run("check", "--stdout", "--config", h.configPath)

	s.Equal(2, code)

	lines := errorSummaryLines(h.stderr.String())
	s.Require().Len(lines, 1, "exactly one ERROR summary line on exit 2")
	s.Regexp(`level=ERROR msg="check failed" unchecked=0 problems=1 delivery_failed=0`, lines[0])

	out := h.stdout.String()
	s.Contains(out, "Firefly III unreachable: ")
	s.NotContains(out, "Missing in Firefly III", "a problem-only digest has no missing section")

	for _, e := range h.events.all() {
		s.False(strings.HasPrefix(e, "bank "), "an unreachable Firefly III must cost zero bank calls: %s", e)
	}
}

// TestScenario2OneBankRateLimitedAnotherWorksReportsBothAndEndsCheckFailed covers spec.md US4
// acceptance scenario 2: one bank returns a rate-limit response (bank.ErrRateLimited, direct from
// the fake provider, so no real retry or sleep is involved) while another bank works; the working
// bank's missing transaction is reported, the rate-limited bank's account is listed unchecked with
// reason "rate limited", and the run ends with the "check failed" outcome (exit 2).
func (s *AcceptanceUS4Suite) TestScenario2OneBankRateLimitedAnotherWorksReportsBothAndEndsCheckFailed() {
	h := s.newRateLimitedBankHarness()

	code := h.run("check", "--stdout", "--config", h.configPath)

	s.Equal(2, code)

	lines := errorSummaryLines(h.stderr.String())
	s.Require().Len(lines, 1, "exactly one ERROR summary line on exit 2")
	s.Regexp(`level=ERROR msg="check failed" unchecked=1 problems=0 delivery_failed=0`, lines[0])

	out := h.stdout.String()
	s.Contains(
		out,
		"- "+us4BankA+" "+redact.MaskIBAN(us4AccountAIBAN)+" ("+us4AccountAName+"): unchecked — rate limited",
		"the rate-limited bank's account is listed unchecked with reason \"rate limited\"",
	)
	s.Contains(out, us4BankBDisplay+" · "+redact.MaskIBAN(us4AccountBIBAN)+" · "+us4AccountBName+" (EUR)",
		"the working bank's own heading is shown")
	s.Contains(out, "- 2026-09-19  -63.12 EUR  ANON US4 GROCERY", "the working bank's missing transaction is reported")
}

// TestScenario3TelegramFailsEmailSucceedsDeliversEmailAndLogsTelegramFailure covers spec.md US4
// acceptance scenario 3, end to end through the real CLI path: with a missing bank transaction to
// report, Telegram delivery fails while email succeeds, so the owner still receives the digest by
// email, the Telegram failure is logged as its own WARN naming channel, recipient and reason
// (FR-028), and the run still exits 1 -- some delivered, as opposed to scenario 4's everyone
// failed. Reuses delivery_test.go's deliveryFactory/deliveryNotifier, so a defect this test finds
// is a defect in check.go's delivery fallback, never a second fake.
func (s *AcceptanceUS4Suite) TestScenario3TelegramFailsEmailSucceedsDeliversEmailAndLogsTelegramFailure() {
	h := s.newDeliveryHarness(map[string][]notify.Result{
		"telegram": {{Recipient: "900006789", Err: errors.New("network timeout")}},
		"email":    {{Recipient: "owner@example.com", Err: nil}},
	})

	code := h.run("check", "--config", h.configPath)

	s.Equal(1, code)
	s.Empty(h.stdout.String())

	lines := warnDeliveryLines(h.stderr.String())
	s.Require().Len(lines, 1, "exactly one WARN for the failed Telegram recipient")
	s.Regexp(`level=WARN msg="delivery failed" channel=telegram recipient=…6789 reason="network timeout"`, lines[0])

	email := h.factory.notifier("email")
	s.Require().NotNil(email)
	s.Require().Len(email.digests, 1, "the owner must still receive the digest by email")
}

// TestScenario4EveryNotifierFailsWritesDigestToErrorOutputAndEndsCheckFailed covers spec.md US4
// acceptance scenario 4, end to end through the real CLI path: when every configured notifier
// fails to deliver, the digest is written to the error output (stderr) and the run ends with the
// "check failed" outcome (exit 2), while each failure still gets its own WARN (FR-028). Reuses
// delivery_test.go's deliveryFactory/deliveryNotifier, so a defect this test finds is a defect in
// check.go's delivery fallback, never a second fake.
func (s *AcceptanceUS4Suite) TestScenario4EveryNotifierFailsWritesDigestToErrorOutputAndEndsCheckFailed() {
	h := s.newDeliveryHarness(map[string][]notify.Result{
		"telegram": {{Recipient: "900006789", Err: errors.New("network timeout")}},
		"email":    {{Recipient: "owner@example.com", Err: errors.New("smtp connection refused")}},
	})

	code := h.run("check", "--config", h.configPath)

	s.Equal(2, code)
	s.Empty(h.stdout.String())

	tg := h.factory.notifier("telegram")
	s.Require().NotNil(tg)
	s.Require().Len(tg.digests, 1)
	s.Contains(h.stderr.String(), tg.digests[0].Text(), "the full digest text must reach the error output")

	warns := warnDeliveryLines(h.stderr.String())
	s.Require().Len(warns, 2, "each failure still gets its own WARN alongside the digest fallback")

	errLines := errorSummaryLines(h.stderr.String())
	s.Require().Len(errLines, 1)
	s.Regexp(`level=ERROR msg="check failed" unchecked=0 problems=0 delivery_failed=2`, errLines[0])
}

// TestScenario5InvalidConfigStopsBeforeAnyExternalCallWithExit2 covers spec.md US4 acceptance
// scenario 5's two named examples: a missing Firefly III address and an unreadable secret file.
// Both stop the command before it contacts any external service (the Factory is never even
// called, exactly as failfast_test.go's FailFastSuite proves for the rest of contracts/config.md's
// validation table), name the problem on stderr, and exit 2. It drives failfast_test.go's own
// newFFHarness/ffPaths (FailFastSuite's own zero-hits harness) rather than a second copy of that
// pattern: ffPaths.fireflyURL defaults to the fake Firefly III server's URL, and
// ffPaths.fireflyTokenFile is already a valid, owner-only secret file a case can chmod unreadable.
func (s *AcceptanceUS4Suite) TestScenario5InvalidConfigStopsBeforeAnyExternalCallWithExit2() {
	cases := []struct {
		name       string
		mutate     func(dir string, p *ffPaths)
		wantSubstr string
		skipRoot   bool
	}{
		{
			name:       "missing Firefly III address",
			mutate:     func(_ string, p *ffPaths) { p.fireflyURL = "" },
			wantSubstr: "firefly.url: required",
		},
		{
			name: "unreadable secret file",
			mutate: func(_ string, p *ffPaths) {
				s.Require().NoError(os.Chmod(p.fireflyTokenFile, 0o000))
				s.T().Cleanup(func() { _ = os.Chmod(p.fireflyTokenFile, cliSecretFileMode) })
			},
			wantSubstr: "firefly.token_file",
			skipRoot:   true,
		},
	}

	for _, tc := range cases {
		s.Run(tc.name, func() {
			if tc.skipRoot && os.Geteuid() == 0 {
				s.T().Skip("root bypasses file permission checks")
			}

			h := newFFHarness(s.T(), s.vilnius, tc.mutate)

			code := h.run("check", "--config", h.configPath)

			s.Equal(2, code)
			s.Contains(h.stderr.String(), tc.wantSubstr, "the message names the problem")
			s.Empty(h.factory.calls(), "an invalid config must never reach the Factory")
			s.Empty(h.events.all(), "zero external hits before the command starts")
		})
	}
}

// newFireflyUnreachableHarness builds scenario 1's fixture: one bank with one account, and a fake
// Firefly III whose accounts endpoint always answers 404 (a 401 or 403 would read "unauthorized").
func (s *AcceptanceUS4Suite) newFireflyUnreachableHarness() *us4Harness {
	s.T().Helper()

	events := &eventLog{}
	ff := &fakeFirefly{events: events, accountsStatus: http.StatusNotFound}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/accounts", ff.serveAccounts)
	mux.HandleFunc("GET /api/v1/accounts/{id}/transactions", ff.serveTransactions)

	ff.srv = httptest.NewServer(mux)
	s.T().Cleanup(ff.srv.Close)

	dir := s.T().TempDir()

	key, err := os.ReadFile(filepath.Clean(cliPrivateKeyFixture))
	s.Require().NoError(err)
	writeSecret(s.T(), filepath.Join(dir, "enablebanking.pem"), string(key))

	statePath := filepath.Join(dir, "state.json")
	s.Require().NoError(state.Save(statePath, &state.State{
		Version: state.CurrentVersion,
		Sessions: map[string]state.Session{
			us4Bank: {
				Provider:     "enablebanking",
				SessionID:    us4SessionID,
				ValidUntil:   time.Date(2027, 3, 20, 0, 0, 0, 0, time.UTC),
				AuthorizedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
				Accounts: []state.Account{
					{
						UID:      us4AccountUID,
						Hash:     us4AccountHash,
						IBAN:     us4AccountIBAN,
						Currency: "EUR",
						Name:     us4AccountName,
					},
				},
			},
		},
	}))

	configPath := filepath.Join(dir, "config.yaml")
	writeSecret(s.T(), configPath, buildConfigYAML(configOpts{
		timezone:       "UTC",
		stateFile:      statePath,
		logFile:        filepath.Join(dir, "firefly-jar.log"),
		fireflyURL:     ff.srv.URL,
		privateKeyFile: filepath.Join(dir, "enablebanking.pem"),
		banks: []configBank{
			{key: us4Bank, name: us4BankName, country: "LT", display: us4BankDisplay},
		},
	}))

	provider := &fakeProvider{events: events, txs: map[string][]bank.Transaction{}}

	return &us4Harness{
		configPath: configPath,
		env:        map[string]string{envFireflyToken: us4FireflyToken},
		firefly:    ff,
		factory:    &us4Factory{provider: provider},
		events:     events,
		stdout:     &bytes.Buffer{},
		stderr:     &bytes.Buffer{},
		clock:      time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC),
	}
}

// newRateLimitedBankHarness builds scenario 2's fixture: two banks, one account each, both
// auto-mapping by IBAN and currency to their own Firefly III asset account with no entries at all
// (fakeFirefly returns an empty page for any account id it was not scripted for). Bank A's account
// always fails with bank.ErrRateLimited; bank B's account has one bank transaction never entered
// in Firefly III.
func (s *AcceptanceUS4Suite) newRateLimitedBankHarness() *us4Harness {
	s.T().Helper()

	body, err := json.Marshal(us4AccountsPage([]us4FFAccount{
		{id: us4FireflyIDA, iban: us4AccountAIBAN, currency: "EUR", name: us4AccountAName},
		{id: us4FireflyIDB, iban: us4AccountBIBAN, currency: "EUR", name: us4AccountBName},
	}))
	s.Require().NoError(err)

	events := &eventLog{}
	ff := &fakeFirefly{events: events, accountsBody: body}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/accounts", ff.serveAccounts)
	mux.HandleFunc("GET /api/v1/accounts/{id}/transactions", ff.serveTransactions)

	ff.srv = httptest.NewServer(mux)
	s.T().Cleanup(ff.srv.Close)

	dir := s.T().TempDir()

	key, err := os.ReadFile(filepath.Clean(cliPrivateKeyFixture))
	s.Require().NoError(err)
	writeSecret(s.T(), filepath.Join(dir, "enablebanking.pem"), string(key))

	statePath := filepath.Join(dir, "state.json")
	s.Require().NoError(state.Save(statePath, &state.State{
		Version: state.CurrentVersion,
		Sessions: map[string]state.Session{
			us4BankA: {
				Provider:     "enablebanking",
				SessionID:    us4SessionA,
				ValidUntil:   time.Date(2027, 3, 20, 0, 0, 0, 0, time.UTC),
				AuthorizedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
				Accounts: []state.Account{
					{
						UID: us4AccountAUID, Hash: us4AccountAHash, IBAN: us4AccountAIBAN, Currency: "EUR",
						Name: us4AccountAName,
					},
				},
			},
			us4BankB: {
				Provider:     "enablebanking",
				SessionID:    us4SessionB,
				ValidUntil:   time.Date(2027, 3, 20, 0, 0, 0, 0, time.UTC),
				AuthorizedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
				Accounts: []state.Account{
					{
						UID: us4AccountBUID, Hash: us4AccountBHash, IBAN: us4AccountBIBAN, Currency: "EUR",
						Name: us4AccountBName,
					},
				},
			},
		},
	}))

	configPath := filepath.Join(dir, "config.yaml")
	writeSecret(s.T(), configPath, buildConfigYAML(configOpts{
		timezone:       "UTC",
		stateFile:      statePath,
		logFile:        filepath.Join(dir, "firefly-jar.log"),
		fireflyURL:     ff.srv.URL,
		privateKeyFile: filepath.Join(dir, "enablebanking.pem"),
		banks: []configBank{
			{key: us4BankA, name: us4BankAName, country: "LT", display: us4BankADisplay},
			{key: us4BankB, name: us4BankBName, country: "LT", display: us4BankBDisplay},
		},
	}))

	provider := &consentProvider{
		events: events,
		scripts: map[string]consentAccountScript{
			us4AccountAUID: {err: bank.ErrRateLimited},
			us4AccountBUID: {
				txs: []bank.Transaction{s.bankTx("2026-09-19", "-63.12", "ref-us4b", "ANON US4 GROCERY")},
			},
		},
	}

	return &us4Harness{
		configPath: configPath,
		env:        map[string]string{envFireflyToken: us4FireflyToken},
		firefly:    ff,
		factory:    &us4Factory{provider: provider},
		events:     events,
		stdout:     &bytes.Buffer{},
		stderr:     &bytes.Buffer{},
		clock:      time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC),
	}
}

// newDeliveryHarness builds one scenario 3/4 case exactly as delivery_test.go's
// DeliverySuite.newHarness does: one bank with one account whose one bank transaction
// (2026-09-19, -63.12 EUR) is always missing, both notify: channels configured, and each
// notifier's Send outcome scripted from results.
func (s *AcceptanceUS4Suite) newDeliveryHarness(results map[string][]notify.Result) *deliveryHarness {
	s.T().Helper()

	accounts, err := os.ReadFile(filepath.Clean(checkAccountsFile))
	s.Require().NoError(err)

	events := &eventLog{}
	ff := &fakeFirefly{events: events, accountsBody: accounts}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/accounts", ff.serveAccounts)
	mux.HandleFunc("GET /api/v1/accounts/{id}/transactions", ff.serveTransactions)

	ff.srv = httptest.NewServer(mux)
	s.T().Cleanup(ff.srv.Close)

	dir := s.T().TempDir()
	h := &deliveryHarness{
		configPath: filepath.Join(dir, "config.yaml"),
		env: map[string]string{
			envFireflyToken:  checkFireflyToken,
			envTelegramToken: cliTelegramToken,
		},
		firefly: ff,
		factory: &deliveryFactory{
			provider: &fakeProvider{
				events: events,
				txs: map[string][]bank.Transaction{
					checkAccountUID: {s.bankTx("2026-09-19", "-63.12", "ref-us4-delivery", "ANON GROCERY")},
				},
			},
			results:   results,
			notifiers: map[string]*deliveryNotifier{},
		},
		stdout: &bytes.Buffer{},
		stderr: &bytes.Buffer{},
		clock:  time.Date(2026, 9, 22, 10, 0, 0, 0, s.vilnius),
	}

	key, err := os.ReadFile(filepath.Clean(cliPrivateKeyFixture))
	s.Require().NoError(err)
	writeSecret(s.T(), filepath.Join(dir, "enablebanking.pem"), string(key))
	writeSecret(s.T(), filepath.Join(dir, "smtp.password"), cliSMTPPassword)

	writeCheckBankState(s.T(), filepath.Join(dir, "state.json"), state.CurrentVersion)
	writeSecret(s.T(), h.configPath, buildConfigYAML(configOpts{
		timezone:       "Europe/Vilnius",
		stateFile:      filepath.Join(dir, "state.json"),
		logFile:        filepath.Join(dir, "firefly-jar.log"),
		fireflyURL:     ff.srv.URL,
		privateKeyFile: filepath.Join(dir, "enablebanking.pem"),
		banks: []configBank{
			{key: checkBankKey, name: "Test Bank", country: "LT", display: "Testbank"},
		},
		telegram:          true,
		telegramTokenFile: filepath.Join(dir, "telegram.token"),
		telegramChatIDs:   []int64{100000001, 900006789},
		email:             true,
		emailPasswordFile: filepath.Join(dir, "smtp.password"),
	}))

	return h
}

// bankTx builds one booked EUR bank.Transaction.
func (s *AcceptanceUS4Suite) bankTx(date, amount, ref, description string) bank.Transaction {
	s.T().Helper()

	value, err := money.ParseAmount(amount, "EUR")
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
