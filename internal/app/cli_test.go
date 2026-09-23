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
	"time"

	"github.com/stretchr/testify/suite"

	"github.com/Toshik1978/firefly-jar/internal/app"
	"github.com/Toshik1978/firefly-jar/internal/bank"
	"github.com/Toshik1978/firefly-jar/internal/civil"
	"github.com/Toshik1978/firefly-jar/internal/config"
	"github.com/Toshik1978/firefly-jar/internal/digest"
	"github.com/Toshik1978/firefly-jar/internal/money"
	"github.com/Toshik1978/firefly-jar/internal/notify"
	"github.com/Toshik1978/firefly-jar/internal/state"
)

// Anonymized secrets and fixtures of the CLI cases. The private key is the committed test key,
// copied into each case's temp dir with owner-only permissions so no permission warning reaches
// stderr whatever mode the checkout gave it.
const (
	cliPrivateKeyFixture = "../../testdata/config/secrets/enablebanking.pem"
	cliTelegramToken     = "test-telegram-token-0000"
	cliSMTPPassword      = "test-smtp-password-0000"
	cliMissingLine       = "- 2026-09-19  -63.12 EUR  ANON GROCERY"
	cliSecretFileMode    = 0o600
)

// Environment variable names the CLI reads through Env.Getenv (contracts/config.md).
const (
	envFireflyToken  = "FIREFLY_JAR_FIREFLY_TOKEN"
	envTelegramToken = "FIREFLY_JAR_TELEGRAM_TOKEN"
)

// cliNotify selects the notify: channels a case's config declares, and whether their secrets are
// provided (Telegram through the environment, SMTP through a password file in the temp dir).
type cliNotify struct {
	telegram bool
	email    bool
	secrets  bool
}

// namedNotifier stands in for a notifier BuildDeps built: it keeps the real channel name, delivers
// successfully to one recipient and records every digest, so nothing is really sent.
type namedNotifier struct {
	name string

	mu      sync.Mutex
	digests []digest.Digest
}

// Name returns the name of the notifier it replaced.
func (n *namedNotifier) Name() string {
	return n.name
}

// Send records d and reports one successful recipient.
func (n *namedNotifier) Send(_ context.Context, d digest.Digest) []notify.Result {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.digests = append(n.digests, d)

	return []notify.Result{{Recipient: "recipient-0001"}}
}

// sent returns a copy of every digest recorded so far.
func (n *namedNotifier) sent() []digest.Digest {
	n.mu.Lock()
	defer n.mu.Unlock()

	return append([]digest.Digest(nil), n.digests...)
}

// cliFactory is the test app.Factory: it records the BuildInput it was given, lets the production
// app.BuildDeps build the real dependencies, records the names of the notifiers BuildDeps built,
// then swaps in the fake bank provider and recording notifiers before handing the deps back.
type cliFactory struct {
	provider *fakeProvider

	mu        sync.Mutex
	inputs    []app.BuildInput
	built     [][]string
	notifiers map[string]*namedNotifier
}

// build is the app.Factory the harness injects.
func (f *cliFactory) build(ctx context.Context, in app.BuildInput) (app.Deps, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.inputs = append(f.inputs, in)

	deps, err := app.BuildDeps(ctx, in)
	if err != nil {
		return app.Deps{}, fmt.Errorf("build deps: %w", err)
	}

	names := make([]string, 0, len(deps.Notifiers))
	fakes := make([]notify.Notifier, 0, len(deps.Notifiers))

	for _, n := range deps.Notifiers {
		fake := &namedNotifier{name: n.Name()}
		f.notifiers[fake.name] = fake
		names = append(names, fake.name)
		fakes = append(fakes, fake)
	}

	f.built = append(f.built, names)
	deps.Notifiers = fakes
	deps.Provider = f.provider

	return deps, nil
}

// calls returns a copy of every BuildInput received so far.
func (f *cliFactory) calls() []app.BuildInput {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]app.BuildInput(nil), f.inputs...)
}

// builtNames returns the notifier names BuildDeps built on each call.
func (f *cliFactory) builtNames() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([][]string(nil), f.built...)
}

// notifier returns the recording notifier that replaced the one named name, or nil.
func (f *cliFactory) notifier(name string) *namedNotifier {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.notifiers[name]
}

// cliHarness is one CLI case: a temp dir holding the config, state, secret files and log file; an
// httptest Firefly III the config points at; the environment map Env.Getenv reads; the factory;
// and the three console streams.
type cliHarness struct {
	configPath string
	logPath    string
	env        map[string]string
	firefly    *fakeFirefly
	factory    *cliFactory
	stdout     *bytes.Buffer
	stderr     *bytes.Buffer
	clock      time.Time
}

// run invokes app.RunEnv with args and the harness's Env.
func (h *cliHarness) run(args ...string) int {
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

// CLISuite covers the CLI entry point (T053, contracts/cli.md, contracts/config.md): the global
// flags, usage and exit 2 on an unknown command or flag, check --help, dispatch of check to
// app.Check through the Factory seam, and the per-command wiring of notifiers by app.BuildDeps. It
// is the RED half of T054: app.RunEnv, app.Env, app.Factory, app.BuildInput and app.BuildDeps do
// not exist yet, and app.Run is a stub that always prints usage and returns 2.
//
// Every config is YAML written into the case's temp dir, pointing firefly.url at an httptest
// Firefly III; env-var secrets come only from the Env.Getenv map, never the process environment.
// The factory calls the production BuildDeps, so the notifiers it reports are the ones BuildDeps
// really built, then replaces the bank provider and the notifiers with fakes.
type CLISuite struct {
	suite.Suite

	vilnius *time.Location
}

// SetupSuite loads the configured time zone once.
func (s *CLISuite) SetupSuite() {
	loc, err := time.LoadLocation("Europe/Vilnius")
	s.Require().NoError(err)

	s.vilnius = loc
}

// TestVersionPrintsTheVersionAndExits0 covers the global --version flag: exit 0, one line
// "firefly-jar <version>" on stdout, nothing on stderr.
func (s *CLISuite) TestVersionPrintsTheVersionAndExits0() {
	var stdout, stderr bytes.Buffer

	code := app.Run([]string{"--version"}, strings.NewReader(""), &stdout, &stderr)

	s.Equal(0, code)
	s.Regexp(`^firefly-jar \S+\n$`, stdout.String())
	s.Empty(stderr.String())
}

// TestUnknownCommandOrFlagPrintsUsageAndExits2 covers contracts/cli.md: an unknown command or flag
// prints usage on stderr, nothing on stdout, and exits 2.
func (s *CLISuite) TestUnknownCommandOrFlagPrintsUsageAndExits2() {
	cases := []struct {
		name string
		args []string
	}{
		{name: "unknown command", args: []string{"bogus"}},
		{name: "unknown global flag", args: []string{"--bogus"}},
		{name: "unknown check flag", args: []string{"check", "--bogus"}},
	}

	for _, tc := range cases {
		s.Run(tc.name, func() {
			var stdout, stderr bytes.Buffer

			code := app.Run(tc.args, strings.NewReader(""), &stdout, &stderr)

			s.Equal(2, code)
			s.Contains(strings.ToLower(stderr.String()), "usage", "usage goes to stderr")
			s.Contains(stderr.String(), "firefly-jar")
			s.Empty(stdout.String(), "nothing goes to stdout")
		})
	}
}

// TestCheckHelpExits0 covers contracts/cli.md: --help on check prints its usage, naming its flags,
// and exits 0 without loading any config or building any dependency.
func (s *CLISuite) TestCheckHelpExits0() {
	h := s.newHarness(cliNotify{telegram: true, secrets: true}, nil, nil)

	code := h.run("check", "--help")

	s.Equal(0, code)

	out := h.stdout.String() + h.stderr.String()
	s.Contains(out, "--stdout")
	s.Contains(out, "--config")
	s.Empty(h.factory.calls(), "help builds no dependency")
}

// TestCheckDispatchesToCheck covers check with --config pointing at a valid config: the Factory is
// called once with the check command's input, the fake bank provider is asked for the configured
// session's account, and a clean run exits 0, silent on stdout and stderr, with its summary in the
// configured log file.
func (s *CLISuite) TestCheckDispatchesToCheck() {
	h := s.newHarness(
		cliNotify{telegram: true, secrets: true},
		[]bank.Transaction{s.bankTx("2026-09-20", "-12.40", "ref-1", "ANON BAKERY")},
		[]ffEntry{{group: "501", kind: "withdrawal", date: "2026-09-21", amount: "12.40", description: "FF BAKERY"}},
	)

	code := h.run("check", "--config", h.configPath)

	s.Equal(0, code)
	s.Empty(h.stdout.String(), "a clean run writes nothing to stdout")
	s.Empty(h.stderr.String(), "a clean run writes nothing to stderr")

	inputs := h.factory.calls()
	s.Require().Len(inputs, 1, "the factory is called once per run")
	s.Equal(config.CmdCheck, inputs[0].Command)
	s.False(inputs[0].Stdout)
	s.Require().NotNil(inputs[0].Config)
	s.Equal(h.firefly.srv.URL+"/api/v1", inputs[0].Config.Firefly.URL)
	s.Equal(checkFireflyToken, inputs[0].Secrets.FireflyToken)
	s.NotNil(inputs[0].Secrets.PrivateKey)
	s.Require().NotNil(inputs[0].State)
	s.Contains(inputs[0].State.Sessions, checkBankKey)

	calls := h.factory.provider.recorded()
	s.Require().Len(calls, 1, "check asks the bank for the one mapped account")
	s.Equal(checkSessionID, calls[0].sessionID)
	s.Equal(checkAccountUID, calls[0].account.UID)
	s.Len(h.firefly.recorded(), 1, "check queries Firefly III through the configured URL")

	logData, err := os.ReadFile(filepath.Clean(h.logPath))
	s.Require().NoError(err, "check writes its log file")
	s.Contains(string(logData), summaryKey, "the run summary goes to the log file")
}

// TestCheckStdoutBuildsNoNotifier covers the check --stdout row of the contracts/config.md table:
// with Telegram and email configured but their token and password files missing (and no env
// vars), no notifier secret is resolved, BuildDeps builds no notifier, and the digest goes to
// stdout with the exit code a sending run would give.
func (s *CLISuite) TestCheckStdoutBuildsNoNotifier() {
	h := s.newHarness(
		cliNotify{telegram: true, email: true, secrets: false},
		[]bank.Transaction{s.bankTx("2026-09-19", "-63.12", "ref-1", "ANON GROCERY")},
		nil,
	)

	code := h.run("check", "--stdout", "--config", h.configPath)

	s.Equal(1, code)
	s.Empty(h.stderr.String())
	s.Contains(h.stdout.String(), checkHeading)
	s.Contains(h.stdout.String(), cliMissingLine)

	inputs := h.factory.calls()
	s.Require().Len(inputs, 1)
	s.Equal(config.CmdCheck, inputs[0].Command)
	s.True(inputs[0].Stdout)
	s.Empty(inputs[0].Secrets.TelegramToken, "check --stdout never reads the Telegram token")
	s.Empty(inputs[0].Secrets.SMTPPassword, "check --stdout never reads the SMTP password")
	built := h.factory.builtNames()
	s.Require().Len(built, 1)
	s.Empty(built[0], "check --stdout builds no notifier")
}

// TestCheckBuildsExactlyTheConfiguredNotifiers covers the check row of the contracts/config.md
// table: without --stdout, BuildDeps builds one notifier per configured channel and no other, the
// digest reaches each of them, and the delivered run exits 1 silently.
func (s *CLISuite) TestCheckBuildsExactlyTheConfiguredNotifiers() {
	cases := []struct {
		name   string
		notify cliNotify
		want   []string
	}{
		{name: "telegram only", notify: cliNotify{telegram: true, secrets: true}, want: []string{"telegram"}},
		{name: "email only", notify: cliNotify{email: true, secrets: true}, want: []string{"email"}},
		{
			name:   "telegram and email",
			notify: cliNotify{telegram: true, email: true, secrets: true},
			want:   []string{"telegram", "email"},
		},
	}

	for _, tc := range cases {
		s.Run(tc.name, func() {
			h := s.newHarness(
				tc.notify,
				[]bank.Transaction{s.bankTx("2026-09-19", "-63.12", "ref-1", "ANON GROCERY")},
				nil,
			)

			code := h.run("check", "--config", h.configPath)

			s.Equal(1, code)
			s.Empty(h.stdout.String(), "a delivered run writes nothing to stdout")
			s.Empty(h.stderr.String(), "a delivered run writes nothing to stderr")

			built := h.factory.builtNames()
			s.Require().Len(built, 1)
			s.ElementsMatch(tc.want, built[0])

			inputs := h.factory.calls()
			s.Require().Len(inputs, 1)
			s.Equal(tc.notify.telegram, inputs[0].Secrets.TelegramToken == cliTelegramToken)
			s.Equal(tc.notify.email, inputs[0].Secrets.SMTPPassword == cliSMTPPassword)

			for _, name := range tc.want {
				n := h.factory.notifier(name)
				s.Require().NotNil(n, name)
				s.Require().Len(n.sent(), 1, name)
				s.Contains(n.sent()[0].Text(), cliMissingLine)
			}
		})
	}
}

// TestCheckWithMissingNotifierSecretExits2 covers the other side of the check row: without
// --stdout, a configured notifier whose secret cannot be resolved is a config error, reported on
// stderr naming the key, exit 2, and no dependency is built.
func (s *CLISuite) TestCheckWithMissingNotifierSecretExits2() {
	h := s.newHarness(
		cliNotify{telegram: true, secrets: false},
		[]bank.Transaction{s.bankTx("2026-09-19", "-63.12", "ref-1", "ANON GROCERY")},
		nil,
	)

	code := h.run("check", "--config", h.configPath)

	s.Equal(2, code)
	s.Contains(h.stderr.String(), "notify.telegram.bot_token_file")
	s.Empty(h.stdout.String())
	s.Empty(h.factory.calls(), "an invalid config builds no dependency")
	s.Empty(h.factory.provider.recorded(), "an invalid config makes no bank call")
}

// newHarness writes a config, a state file with one session and the needed secret files into a
// fresh temp dir, and starts a fake Firefly III serving entries on account #1. The fake provider
// returns bankTxs for the session's one account.
func (s *CLISuite) newHarness(n cliNotify, bankTxs []bank.Transaction, entries []ffEntry) *cliHarness {
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
		env:        map[string]string{envFireflyToken: checkFireflyToken},
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

	if n.secrets {
		h.env[envTelegramToken] = cliTelegramToken
		s.writeSecret(filepath.Join(dir, "smtp.password"), cliSMTPPassword)
	}

	s.writeState(filepath.Join(dir, "state.json"))
	s.writeSecret(h.configPath, s.configYAML(dir, ff.srv.URL, n))

	return h
}

// configYAML renders the case's config. Secret file paths that the case does not provide name
// files that do not exist, so reading them would fail.
func (*CLISuite) configYAML(dir, fireflyURL string, n cliNotify) string {
	return buildConfigYAML(configOpts{
		timezone:          "Europe/Vilnius",
		stateFile:         filepath.Join(dir, "state.json"),
		logFile:           filepath.Join(dir, "firefly-jar.log"),
		fireflyURL:        fireflyURL,
		privateKeyFile:    filepath.Join(dir, "enablebanking.pem"),
		banks:             []configBank{{key: checkBankKey, name: "Test Bank", country: "LT", display: "Testbank"}},
		telegram:          n.telegram,
		telegramTokenFile: filepath.Join(dir, "telegram.token"),
		telegramChatIDs:   []int64{100000001},
		email:             n.email,
		emailPasswordFile: filepath.Join(dir, "smtp.password"),
	})
}

// writeState saves a state file with one session for the configured bank whose one account maps
// automatically to Firefly III account #1.
func (s *CLISuite) writeState(path string) {
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
func (s *CLISuite) writeSecret(path, content string) {
	s.T().Helper()

	s.Require().NoError(os.WriteFile(path, []byte(content), cliSecretFileMode))
}

// bankTx builds one booked EUR bank transaction.
func (s *CLISuite) bankTx(date, amount, ref, description string) bank.Transaction {
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
