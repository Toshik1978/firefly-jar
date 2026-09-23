package app_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	"github.com/Toshik1978/firefly-jar/internal/app"
	"github.com/Toshik1978/firefly-jar/internal/bank"
	"github.com/Toshik1978/firefly-jar/internal/civil"
	"github.com/Toshik1978/firefly-jar/internal/money"
	"github.com/Toshik1978/firefly-jar/internal/notify"
	"github.com/Toshik1978/firefly-jar/internal/state"
)

// ffCommand is one subcommand FailFastSuite drives against the same broken-or-fixed config: check,
// auth (which needs the bank key as its argument) and accounts. These are exactly the three
// commands contracts/cli.md validates config for before any network call.
type ffCommand struct {
	name string
	args []string
}

// ffAllCommands returns check, auth <bankKey> and accounts.
func ffAllCommands(bankKey string) []ffCommand {
	return []ffCommand{
		{name: "check", args: []string{"check"}},
		{name: "auth", args: []string{"auth", bankKey}},
		{name: "accounts", args: []string{"accounts"}},
	}
}

// ffPaths names every path a FailFastSuite config points at. newFFHarness fills every field with a
// valid file or directory before handing it to a case's mutate callback, so a case only has to break
// the one path (or set unknownKey) its own case is about; every other path stays valid, and the
// failure it observes can only come from the rule under test. fireflyURL defaults to the fake
// Firefly III server's own URL (set by newFFHarness before mutate runs), so a case can blank it out
// to cover "missing Firefly III address" without needing its own harness (acceptance_us4_test.go's
// AcceptanceUS4Suite scenario 5).
type ffPaths struct {
	fireflyURL        string
	stateFile         string
	logFile           string
	privateKeyFile    string
	fireflyTokenFile  string
	telegramTokenFile string
	smtpPasswordFile  string
	telegram          bool
	email             bool
	unknownKey        bool
}

// ffConfigYAML renders one FailFastSuite config, via the shared buildConfigYAML every suite in this
// package now uses.
func ffConfigYAML(p ffPaths) string {
	var extraTop []string
	if p.unknownKey {
		extraTop = []string{"bogus_setting: true"}
	}

	return buildConfigYAML(configOpts{
		extraTop:         extraTop,
		timezone:         "Europe/Vilnius",
		stateFile:        p.stateFile,
		logFile:          p.logFile,
		fireflyURL:       p.fireflyURL,
		fireflyTokenFile: p.fireflyTokenFile,
		privateKeyFile:   p.privateKeyFile,
		banks: []configBank{
			{key: checkBankKey, name: "Test Bank", country: "LT", display: "Testbank"},
		},
		telegram:          p.telegram,
		telegramTokenFile: p.telegramTokenFile,
		telegramChatIDs:   []int64{100000001},
		email:             p.email,
		emailPasswordFile: p.smtpPasswordFile,
	})
}

// ffFactory is the app.Factory FailFastSuite injects for every command it drives. It lets the
// production app.BuildDeps build everything for real, so a case's exit code and stderr come from
// the CLI's own validation gate rather than a test shortcut, then swaps in a fake for whichever of
// Deps.Authorizer, Deps.Provider and Deps.Notifiers BuildDeps actually built for that command,
// exactly as cli_test.go's cliFactory and auth_test.go's authFactory already do for their own
// suites. Recording every BuildInput it receives is what proves the "zero hits" half of T074: a
// case whose validation is supposed to fail asserts this factory was never called at all, so none
// of the Firefly, Enable Banking, Telegram or SMTP clients BuildDeps would have built ever had a
// chance to be hit.
type ffFactory struct {
	authorizer *fakeAuthorizer
	provider   *fakeProvider

	mu        sync.Mutex
	inputs    []app.BuildInput
	built     [][]string
	notifiers map[string]*namedNotifier
}

// build is the app.Factory the harness injects.
func (f *ffFactory) build(ctx context.Context, in app.BuildInput) (app.Deps, error) {
	f.mu.Lock()
	f.inputs = append(f.inputs, in)
	f.mu.Unlock()

	deps, err := app.BuildDeps(ctx, in)
	if err != nil {
		return app.Deps{}, fmt.Errorf("build deps: %w", err)
	}

	if deps.Authorizer != nil {
		deps.Authorizer = f.authorizer
	}

	if deps.Provider != nil {
		deps.Provider = f.provider
	}

	names := make([]string, 0, len(deps.Notifiers))
	fakes := make([]notify.Notifier, 0, len(deps.Notifiers))

	f.mu.Lock()
	for _, n := range deps.Notifiers {
		fake := &namedNotifier{name: n.Name()}
		f.notifiers[fake.name] = fake
		names = append(names, fake.name)
		fakes = append(fakes, fake)
	}
	f.built = append(f.built, names)
	f.mu.Unlock()

	deps.Notifiers = fakes

	return deps, nil
}

// calls returns a copy of every BuildInput received so far.
func (f *ffFactory) calls() []app.BuildInput {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]app.BuildInput(nil), f.inputs...)
}

// builtNames returns the notifier names BuildDeps built on each call.
func (f *ffFactory) builtNames() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([][]string(nil), f.built...)
}

// ffHarness is one FailFastSuite case's environment: a temp dir with every secret and state file
// filled in valid unless a case's mutate callback broke one, a fake Firefly III (doubling as the
// bank-provider event log's other half through the shared eventLog), and the ffFactory.
type ffHarness struct {
	dir        string
	configPath string
	firefly    *fakeFirefly
	events     *eventLog
	factory    *ffFactory
	stdout     *bytes.Buffer
	stderr     *bytes.Buffer
	clock      time.Time
	stdin      string
}

// run invokes app.RunEnv with args and the harness's Env.
func (h *ffHarness) run(args ...string) int {
	return app.RunEnv(args, app.Env{
		Stdin:     strings.NewReader(h.stdin),
		Stdout:    h.stdout,
		Stderr:    h.stderr,
		Getenv:    func(string) string { return "" },
		Now:       func() time.Time { return h.clock },
		Transport: h.firefly.srv.Client().Transport,
		Factory:   h.factory.build,
	})
}

// runCommand runs c against the harness's config.
func (h *ffHarness) runCommand(c ffCommand) int {
	args := append(append([]string{}, c.args...), "--config", h.configPath)

	return h.run(args...)
}

// FailFastSuite is the RED half of T075 (design fj-xwu.6.5, contracts/cli.md, contracts/config.md,
// contracts/state.md): config and state must be fully validated, per the command-scoped table of
// contracts/config.md, before any client is built, so an owner's typo never costs a PSD2 quota hit
// or reaches a notifier. Every case drives the real CLI path app.RunEnv through the ffFactory, which
// lets the production app.BuildDeps run for real before swapping in fakes, so "zero hits" and "still
// succeeds" are both proved against the genuine validation and command logic, never a test
// shortcut.
//
// Per the bead's ruling, a case whose behaviour already exists from earlier phases (config load,
// ValidateFor's per-command secret resolution, state.Load's version check) may already pass: this
// suite still pins it, and the implementation task's report carries the mutation proof for each such
// case instead of a failing run.
type FailFastSuite struct {
	suite.Suite

	vilnius *time.Location
}

// SetupSuite loads the configured time zone once.
func (s *FailFastSuite) SetupSuite() {
	loc, err := time.LoadLocation("Europe/Vilnius")
	s.Require().NoError(err)

	s.vilnius = loc
}

// TestUnknownConfigKeyExitsTwoWithZeroHits covers the first universal case of the design: an
// unknown top-level config key fails check, auth and accounts alike, naming the key on stderr, and
// none of them ever reaches the Factory.
func (s *FailFastSuite) TestUnknownConfigKeyExitsTwoWithZeroHits() {
	for _, tc := range ffAllCommands(checkBankKey) {
		s.Run(tc.name, func() {
			h := s.newFFHarness(func(_ string, p *ffPaths) { p.unknownKey = true })

			code := h.runCommand(tc)

			s.Equal(2, code)
			s.Contains(h.stderr.String(), "bogus_setting")
			s.assertZeroHits(h)
		})
	}
}

// TestBadOrMissingPrivateKeyExitsTwoWithZeroHits covers the second universal case: an Enable
// Banking private key that is missing, or present but not a parseable RSA PEM, fails all three
// commands, since resolvePrivateKey runs outside ValidateFor's per-command switch.
func (s *FailFastSuite) TestBadOrMissingPrivateKeyExitsTwoWithZeroHits() {
	keyCases := []struct {
		name       string
		mutate     func(dir string, p *ffPaths)
		wantSubstr string
	}{
		{
			name:       "missing",
			mutate:     func(dir string, p *ffPaths) { p.privateKeyFile = filepath.Join(dir, "no-such-key.pem") },
			wantSubstr: "enablebanking.private_key_file",
		},
		{
			name: "malformed",
			mutate: func(_ string, p *ffPaths) {
				s.Require().NoError(os.WriteFile(p.privateKeyFile, []byte("not a pem file\n"), cliSecretFileMode))
			},
			wantSubstr: "enablebanking private key",
		},
	}

	for _, kc := range keyCases {
		for _, tc := range ffAllCommands(checkBankKey) {
			s.Run(kc.name+"/"+tc.name, func() {
				h := s.newFFHarness(kc.mutate)

				code := h.runCommand(tc)

				s.Equal(2, code)
				s.Contains(h.stderr.String(), kc.wantSubstr)
				s.assertZeroHits(h)
			})
		}
	}
}

// TestStateVersion2ExitsTwoWithZeroHits covers the third universal case: contracts/state.md's rule
// that an unknown state file version fails the run, before any client is built, for all three
// commands.
func (s *FailFastSuite) TestStateVersion2ExitsTwoWithZeroHits() {
	for _, tc := range ffAllCommands(checkBankKey) {
		s.Run(tc.name, func() {
			h := s.newFFHarness(func(_ string, p *ffPaths) { writeCheckBankState(s.T(), p.stateFile, 2) })

			code := h.runCommand(tc)

			s.Equal(2, code)
			s.Contains(h.stderr.String(), "unknown version 2")
			s.assertZeroHits(h)
		})
	}
}

// TestMissingFireflyTokenFailsAccountsAndCheckNotAuth covers the first command-scoped row of
// contracts/config.md: accounts and check both need the Firefly token, but auth never resolves it,
// so a missing token file must not block auth at all. The auth sub-case runs its full flow to a
// genuine exit 0, rather than merely observing that no error surfaced, so a regression that made
// auth resolve the token too still turns this into a failing exit 2 with the token error on stderr.
func (s *FailFastSuite) TestMissingFireflyTokenFailsAccountsAndCheckNotAuth() {
	mutate := func(dir string, p *ffPaths) { p.fireflyTokenFile = filepath.Join(dir, "no-such-token") }

	s.Run("check fails", func() {
		h := s.newFFHarness(mutate)

		code := h.run("check", "--config", h.configPath)

		s.Equal(2, code)
		s.Contains(h.stderr.String(), "firefly.token_file")
		s.assertZeroHits(h)
	})

	s.Run("accounts fails", func() {
		h := s.newFFHarness(mutate)

		code := h.run("accounts", "--config", h.configPath)

		s.Equal(2, code)
		s.Contains(h.stderr.String(), "firefly.token_file")
		s.assertZeroHits(h)
	})

	s.Run("auth still succeeds", func() {
		h := s.newFFHarness(mutate)

		code := h.run("auth", checkBankKey, "--config", h.configPath)

		s.Equal(0, code, h.stderr.String())
		s.NotContains(h.stderr.String(), "firefly.token_file")
		s.Require().Len(h.factory.calls(), 1, "auth's own validation must succeed, so the Factory is called")
		s.Require().Len(h.factory.authorizer.completeRecorded(), 1, "auth runs its full flow, not just validation")
	})
}

// TestMissingNotifierSecretFailsCheckOnlyNotStdoutAuthOrAccounts covers the second command-scoped
// row: a configured notifier's secret is required only by check without --stdout. Each sub-case
// still runs the other three combinations to a genuine completion rather than only checking the
// absence of an error.
func (s *FailFastSuite) TestMissingNotifierSecretFailsCheckOnlyNotStdoutAuthOrAccounts() {
	cases := []struct {
		name       string
		mutate     func(dir string, p *ffPaths)
		wantSubstr string
	}{
		{
			name:       "telegram token",
			mutate:     func(dir string, p *ffPaths) { p.telegramTokenFile = filepath.Join(dir, "no-such-token") },
			wantSubstr: "notify.telegram.bot_token_file",
		},
		{
			name:       "smtp password",
			mutate:     func(dir string, p *ffPaths) { p.smtpPasswordFile = filepath.Join(dir, "no-such-password") },
			wantSubstr: "notify.email.password_file",
		},
	}

	for _, tc := range cases {
		s.Run(tc.name, func() {
			s.Run("check fails", func() {
				h := s.newFFHarness(tc.mutate)

				code := h.run("check", "--config", h.configPath)

				s.Equal(2, code)
				s.Contains(h.stderr.String(), tc.wantSubstr)
				s.assertZeroHits(h)
			})

			s.Run("check --stdout still succeeds", func() {
				h := s.newFFHarness(tc.mutate)

				code := h.run("check", "--stdout", "--config", h.configPath)

				s.Equal(0, code, h.stderr.String())
				s.Empty(h.stderr.String())
				s.Require().Len(h.factory.calls(), 1)
				s.Require().Len(h.events.all(), 3, "check --stdout still reconciles for real")
			})

			s.Run("auth still succeeds", func() {
				h := s.newFFHarness(tc.mutate)

				code := h.run("auth", checkBankKey, "--config", h.configPath)

				s.Equal(0, code, h.stderr.String())
				s.Require().Len(h.factory.authorizer.completeRecorded(), 1)
			})

			s.Run("accounts still succeeds", func() {
				h := s.newFFHarness(tc.mutate)

				code := h.run("accounts", "--config", h.configPath)

				s.Equal(0, code, h.stderr.String())
				s.Require().Len(h.factory.calls(), 1)
			})
		})
	}
}

// TestUnwritableLogFileFailsCheckOnlyNotAuthOrAccounts covers the third command-scoped row:
// log_file's directory is validated, and required, only for check; auth and accounts never look at
// it, so they still run to a genuine exit 0.
func (s *FailFastSuite) TestUnwritableLogFileFailsCheckOnlyNotAuthOrAccounts() {
	s.skipIfRoot()

	mutate := func(dir string, p *ffPaths) {
		unwritable := filepath.Join(dir, "unwritable-log")
		s.Require().NoError(os.Mkdir(unwritable, 0o500))
		s.T().Cleanup(func() { _ = os.Chmod(unwritable, 0o700) })
		p.logFile = filepath.Join(unwritable, "firefly-jar.log")
	}

	s.Run("check fails", func() {
		h := s.newFFHarness(mutate)

		code := h.run("check", "--config", h.configPath)

		s.Equal(2, code)
		s.Contains(h.stderr.String(), "log_file: directory is not writable")
		s.assertZeroHits(h)
	})

	s.Run("auth still succeeds", func() {
		h := s.newFFHarness(mutate)

		code := h.run("auth", checkBankKey, "--config", h.configPath)

		s.Equal(0, code, h.stderr.String())
		s.Require().Len(h.factory.authorizer.completeRecorded(), 1)
	})

	s.Run("accounts still succeeds", func() {
		h := s.newFFHarness(mutate)

		code := h.run("accounts", "--config", h.configPath)

		s.Equal(0, code, h.stderr.String())
		s.Require().Len(h.factory.calls(), 1)
	})
}

// TestSecretFileMode0644WarnsAndContinues covers the design's last bullet: a secret file readable
// by group or other produces a WARN naming it, and the run still completes, rather than failing.
func (s *FailFastSuite) TestSecretFileMode0644WarnsAndContinues() {
	h := s.newFFHarness(func(_ string, p *ffPaths) {
		s.Require().NoError(os.Chmod(p.fireflyTokenFile, 0o644))
	})

	code := h.run("check", "--config", h.configPath)

	s.Equal(0, code, h.stderr.String())
	s.Contains(h.stderr.String(), "firefly.token_file")
	s.Contains(h.stderr.String(), "chmod 600")
	s.Require().Len(h.factory.calls(), 1, "a permission warning must not stop the run")
	s.Require().Len(h.events.all(), 3, "the run continues through to reconciliation")
}

// assertZeroHits proves a case's failure happened before any client was built: the Factory itself
// was never called, so neither the Firefly III nor the bank-provider (Enable Banking) fake ever saw
// a request, the Enable Banking authorize fake never saw a Begin call, and no notifier (so neither
// the Telegram nor the SMTP client) was ever built.
func (s *FailFastSuite) assertZeroHits(h *ffHarness) {
	s.T().Helper()

	s.Empty(h.factory.calls(), "an invalid config must never reach the Factory, so no client is ever built")
	s.Empty(h.events.all(), "zero Firefly III and bank-provider (Enable Banking) hits")
	s.Empty(h.factory.authorizer.beginRecorded(), "zero Enable Banking authorize hits")
	s.Empty(h.factory.builtNames(), "zero notifiers built, so zero Telegram/SMTP hits")
}

// skipIfRoot skips permission-denial cases, which root bypasses.
func (s *FailFastSuite) skipIfRoot() {
	if os.Geteuid() == 0 {
		s.T().Skip("root bypasses file permission checks")
	}
}

// newFFHarness is FailFastSuite's own entry to the package-level newFFHarness (shared with
// acceptance_us4_test.go's AcceptanceUS4Suite scenario 5), fixed to this suite's own clock.
func (s *FailFastSuite) newFFHarness(mutate func(dir string, p *ffPaths)) *ffHarness {
	return newFFHarness(s.T(), s.vilnius, mutate)
}

// newFFHarness builds one case's temp dir: every ffPaths field filled in valid (including a state
// file carrying one session for checkBankKey whose one account matches checkAccountsFile's Firefly
// III account #1), a fake Firefly III serving that fixture plus one transaction group matching the
// fake bank provider's one scripted transaction, so an otherwise-valid config reaches a real, quiet,
// exit-0 completion. mutate then breaks exactly the one path (or sets unknownKey) the case is about.
// It is a package-level function, not a suite method, so both FailFastSuite and
// acceptance_us4_test.go's AcceptanceUS4Suite (spec.md US4 scenario 5) can drive the exact same
// fail-fast harness rather than each keeping its own near-identical copy.
func newFFHarness(t *testing.T, vilnius *time.Location, mutate func(dir string, p *ffPaths)) *ffHarness {
	t.Helper()

	accounts, err := os.ReadFile(filepath.Clean(checkAccountsFile))
	require.NoError(t, err)

	events := &eventLog{}
	ff := &fakeFirefly{
		events:       events,
		accountsBody: accounts,
		entries: []ffEntry{
			{group: "501", kind: "withdrawal", date: "2026-09-21", amount: "12.40", description: "FF BAKERY"},
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/accounts", ff.serveAccounts)
	mux.HandleFunc("GET /api/v1/accounts/{id}/transactions", ff.serveTransactions)

	ff.srv = httptest.NewServer(mux)
	t.Cleanup(ff.srv.Close)

	dir := t.TempDir()
	paths := ffValidPaths(t, dir)
	paths.fireflyURL = ff.srv.URL

	if mutate != nil {
		mutate(dir, &paths)
	}

	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "config.yaml"), []byte(ffConfigYAML(paths)), cliSecretFileMode,
	))

	amount, err := money.ParseAmount("-12.40", "EUR")
	require.NoError(t, err)

	txDate, err := civil.ParseDate("2026-09-20")
	require.NoError(t, err)

	return &ffHarness{
		dir:        dir,
		configPath: filepath.Join(dir, "config.yaml"),
		firefly:    ff,
		events:     events,
		factory: &ffFactory{
			authorizer: &fakeAuthorizer{statePath: paths.stateFile, session: ffAuthSession()},
			provider: &fakeProvider{
				events: events,
				txs: map[string][]bank.Transaction{
					checkAccountUID: {
						{
							Date:        txDate,
							Amount:      amount,
							Status:      bank.Booked,
							EntryRef:    "ref-1",
							Description: "ANON BAKERY",
						},
					},
				},
			},
			notifiers: map[string]*namedNotifier{},
		},
		stdout: &bytes.Buffer{},
		stderr: &bytes.Buffer{},
		clock:  time.Date(2026, 9, 22, 10, 0, 0, 0, vilnius),
		stdin:  authPastedRedirect + "\n",
	}
}

// ffValidPaths returns every FailFastSuite path filled in with a valid file or directory rooted at
// dir, plus valid owner-only secret files and a state file at state.CurrentVersion carrying one
// session for checkBankKey. fireflyURL is left empty; newFFHarness fills it in with the fake
// server's own URL once it exists.
func ffValidPaths(t *testing.T, dir string) ffPaths {
	t.Helper()

	logDir := filepath.Join(dir, "log")
	require.NoError(t, os.Mkdir(logDir, 0o700))

	key, err := os.ReadFile(filepath.Clean(cliPrivateKeyFixture))
	require.NoError(t, err)

	p := ffPaths{
		stateFile:         filepath.Join(dir, "state.json"),
		logFile:           filepath.Join(logDir, "firefly-jar.log"),
		privateKeyFile:    filepath.Join(dir, "enablebanking.pem"),
		fireflyTokenFile:  filepath.Join(dir, "firefly.token"),
		telegramTokenFile: filepath.Join(dir, "telegram.token"),
		smtpPasswordFile:  filepath.Join(dir, "smtp.password"),
		telegram:          true,
		email:             true,
	}

	writeSecret(t, p.privateKeyFile, string(key))
	writeSecret(t, p.fireflyTokenFile, checkFireflyToken)
	writeSecret(t, p.telegramTokenFile, cliTelegramToken)
	writeSecret(t, p.smtpPasswordFile, cliSMTPPassword)
	writeCheckBankState(t, p.stateFile, state.CurrentVersion)

	return p
}

// ffAuthSession is the session the fake authorizer's Complete returns for an auth "still succeeds"
// sub-case: a different session id and account than the one already in state, so a case can tell the
// run really replaced it rather than leaving the old session untouched.
func ffAuthSession() state.Session {
	return state.Session{
		Provider:     "enablebanking",
		SessionID:    "00000000-0000-0000-0000-0000000000ff",
		ValidUntil:   time.Date(2027, 3, 21, 10, 15, 0, 0, time.UTC),
		AuthorizedAt: time.Date(2026, 9, 23, 9, 0, 0, 0, time.UTC),
		Accounts: []state.Account{{
			UID:      "00000000-0000-0000-0000-000000000900",
			Hash:     "EEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEE=.FFA1",
			IBAN:     "LT000000000000000900",
			Currency: "EUR",
			Name:     "Auth Test",
		}},
	}
}
