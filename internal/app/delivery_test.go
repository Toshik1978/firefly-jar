package app_test

import (
	"bytes"
	"context"
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
	"github.com/Toshik1978/firefly-jar/internal/digest"
	"github.com/Toshik1978/firefly-jar/internal/domain"
	"github.com/Toshik1978/firefly-jar/internal/notify"
	"github.com/Toshik1978/firefly-jar/internal/state"
)

// deliveryNotifier is a fake notify.Notifier for DeliverySuite: it returns exactly the results it
// was scripted with, whatever digest it is asked to send, and records every digest it saw so a
// case can compare stderr against the exact digest.Text() the run produced (FR-028's "the digest
// MUST be written to the error output" fallback). FanOut calls Send once per notifier, in order,
// never concurrently, so no locking is needed.
type deliveryNotifier struct {
	name    string
	results []notify.Result
	digests []digest.Digest
}

// Name returns the channel name this fake stands in for ("telegram" or "email").
func (n *deliveryNotifier) Name() string {
	return n.name
}

// Send records d and returns the results n was scripted with.
func (n *deliveryNotifier) Send(_ context.Context, d digest.Digest) []notify.Result {
	n.digests = append(n.digests, d)

	return n.results
}

// deliveryFactory is the test app.Factory: it lets the production app.BuildDeps build everything
// (so the config and secrets are validated for real), then replaces each notifier BuildDeps built
// with a deliveryNotifier scripted from results, keyed by channel name, and replaces the bank
// provider with a fake that always reports one missing transaction, so every case has a digest to
// deliver.
type deliveryFactory struct {
	provider *fakeProvider
	results  map[string][]notify.Result

	notifiers map[string]*deliveryNotifier
}

// build is the app.Factory the harness injects.
func (f *deliveryFactory) build(ctx context.Context, in app.BuildInput) (app.Deps, error) {
	deps, err := app.BuildDeps(ctx, in)
	if err != nil {
		return app.Deps{}, fmt.Errorf("build deps: %w", err)
	}

	fakes := make([]notify.Notifier, 0, len(deps.Notifiers))

	for _, n := range deps.Notifiers {
		fake := &deliveryNotifier{name: n.Name(), results: f.results[n.Name()]}
		f.notifiers[fake.name] = fake
		fakes = append(fakes, fake)
	}

	deps.Notifiers = fakes
	deps.Provider = f.provider

	return deps, nil
}

// notifier returns the deliveryNotifier that replaced the one named name, or nil.
func (f *deliveryFactory) notifier(name string) *deliveryNotifier {
	return f.notifiers[name]
}

// deliveryHarness is one DeliverySuite case: a temp dir holding the config, state and secret
// files a real check run needs to pass validation, an httptest Firefly III reporting no
// transactions for the mapped account (so the bank's one transaction is always missing), the
// factory, and the console buffers.
type deliveryHarness struct {
	configPath string
	env        map[string]string
	firefly    *fakeFirefly
	factory    *deliveryFactory
	stdout     *bytes.Buffer
	stderr     *bytes.Buffer
	clock      time.Time
}

// run invokes app.RunEnv with args and the harness's Env.
func (h *deliveryHarness) run(args ...string) int {
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

// DeliverySuite covers FR-028's delivery fallback in `check` (T072, RED half of T073): a WARN per
// failed recipient, whatever else succeeded, and, only once every recipient across every channel
// failed, the full digest text on stderr alongside the existing ERROR summary line. It drives the
// real CLI path app.RunEnv -> app.Check, with two notifiers ("telegram", "email") whose Send
// results are scripted per case through the Factory, so a case can pin exactly which recipients
// succeed and which fail without any real network call.
//
// It is red until T073: check.go's deliver only stores notify.FanOut's report.Delivery today, so
// nothing yet logs a WARN per failure or falls back to printing the digest.
type DeliverySuite struct {
	suite.Suite

	vilnius *time.Location
}

// SetupSuite loads the configured time zone once.
func (s *DeliverySuite) SetupSuite() {
	loc, err := time.LoadLocation("Europe/Vilnius")
	s.Require().NoError(err)

	s.vilnius = loc
}

// TestTelegramFailsEmailSucceedsWarnsOnceAndExits1 pins the partial-failure row of contracts/
// cli.md: one failed recipient among several that were attempted logs exactly one WARN naming its
// channel and its masked recipient, in the order channel, recipient, reason
// (`msg="delivery failed" channel=telegram recipient=…6789 reason="network timeout"`), the run
// still exits 1 because the missing transaction was still delivered to email, stdout stays empty,
// and the digest itself never reaches stderr since delivery did not fail for every recipient.
func (s *DeliverySuite) TestTelegramFailsEmailSucceedsWarnsOnceAndExits1() {
	h := s.newHarness(map[string][]notify.Result{
		"telegram": {{Recipient: "900006789", Err: errors.New("network timeout")}},
		"email":    {{Recipient: "owner@example.com", Err: nil}},
	})

	code := h.run("check", "--config", h.configPath)

	s.Equal(1, code)
	s.Empty(h.stdout.String(), "a delivered run writes nothing to stdout")

	lines := warnDeliveryLines(h.stderr.String())
	s.Require().Len(lines, 1, "exactly one WARN for the one failed recipient")
	s.Regexp(`level=WARN msg="delivery failed" channel=telegram recipient=…6789 reason="network timeout"`, lines[0])

	s.Empty(errorSummaryLines(h.stderr.String()), "exit 1 never carries the ERROR summary line")

	tg := h.factory.notifier("telegram")
	s.Require().NotNil(tg)
	s.Require().Len(tg.digests, 1)
	s.NotContains(h.stderr.String(), tg.digests[0].Text(),
		"the digest itself must not reach stderr unless every recipient failed")
}

// TestEveryRecipientFailsPrintsFullDigestAndExits2 pins FR-028's fallback: when delivery failed
// for every recipient across every channel, the full digest text (d.Text(), captured from what
// the fake notifiers actually received) is written to stderr, the existing ERROR summary line
// still carries the right delivery_failed count, and each failure still gets its own WARN
// (FR-028's "each failure MUST be logged as a warning" applies regardless of how many others also
// failed).
func (s *DeliverySuite) TestEveryRecipientFailsPrintsFullDigestAndExits2() {
	h := s.newHarness(map[string][]notify.Result{
		"telegram": {{Recipient: "900006789", Err: errors.New("network timeout")}},
		"email":    {{Recipient: "owner@example.com", Err: errors.New("smtp connection refused")}},
	})

	code := h.run("check", "--config", h.configPath)

	s.Equal(2, code)
	s.Empty(h.stdout.String())

	tg := h.factory.notifier("telegram")
	s.Require().NotNil(tg)
	s.Require().Len(tg.digests, 1)

	s.Contains(h.stderr.String(), tg.digests[0].Text(),
		"the full digest text must reach stderr when every recipient failed")

	errLines := errorSummaryLines(h.stderr.String())
	s.Require().Len(errLines, 1)
	s.Regexp(`level=ERROR msg="check failed" unchecked=0 problems=0 delivery_failed=2`, errLines[0])

	warns := warnDeliveryLines(h.stderr.String())
	s.Require().Len(warns, 2, "both failures still get their own WARN alongside the digest fallback")
}

// TestPartialFailureWithinOneChannelDeliversTheOtherAndWarnsOnce pins the within-channel case: of
// telegram's two chat ids, one delivers and one fails; the failed one still gets its own WARN, the
// delivered one gets none, and the run exits 1 since at least one recipient overall (both the
// other chat id and email) still received the digest.
func (s *DeliverySuite) TestPartialFailureWithinOneChannelDeliversTheOtherAndWarnsOnce() {
	h := s.newHarness(map[string][]notify.Result{
		"telegram": {
			{Recipient: "100000001", Err: nil},
			{Recipient: "900006789", Err: errors.New("network timeout")},
		},
		"email": {{Recipient: "owner@example.com", Err: nil}},
	})

	code := h.run("check", "--config", h.configPath)

	s.Equal(1, code)
	s.Empty(h.stdout.String(), "a delivered run writes nothing to stdout")

	lines := warnDeliveryLines(h.stderr.String())
	s.Require().Len(lines, 1, "only the failed chat id gets a WARN; the delivered one does not")
	s.Regexp(`level=WARN msg="delivery failed" channel=telegram recipient=…6789 reason="network timeout"`, lines[0])

	s.Empty(errorSummaryLines(h.stderr.String()), "exit 1 never carries the ERROR summary line")

	tg := h.factory.notifier("telegram")
	s.Require().NotNil(tg)
	s.Require().Len(tg.digests, 1)
	s.NotContains(h.stderr.String(), tg.digests[0].Text(),
		"the digest itself must not reach stderr unless every recipient failed")
}

// newHarness writes a config, a state file with one session and the needed secret files into a
// fresh temp dir, and starts a fake Firefly III reporting no entries for the mapped account, so
// the one bank transaction it scripts is always missing and a digest is always needed. results
// scripts each configured notifier's Send outcome, keyed by channel name.
func (s *DeliverySuite) newHarness(results map[string][]notify.Result) *deliveryHarness {
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
					checkAccountUID: {s.bankTx("2026-09-19", "-63.12", "ref-1", "ANON GROCERY")},
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
	writeSecret(s.T(), h.configPath, s.configYAML(dir, ff.srv.URL))

	return h
}

// configYAML renders one config with both notify channels configured, so every case exercises
// both "telegram" and "email" through the Factory.
func (*DeliverySuite) configYAML(dir, fireflyURL string) string {
	return buildConfigYAML(configOpts{
		timezone:       "Europe/Vilnius",
		stateFile:      filepath.Join(dir, "state.json"),
		logFile:        filepath.Join(dir, "firefly-jar.log"),
		fireflyURL:     fireflyURL,
		privateKeyFile: filepath.Join(dir, "enablebanking.pem"),
		banks: []configBank{
			{key: checkBankKey, name: "Test Bank", country: "LT", display: "Testbank"},
		},
		telegram:          true,
		telegramTokenFile: filepath.Join(dir, "telegram.token"),
		telegramChatIDs:   []int64{100000001, 900006789},
		email:             true,
		emailPasswordFile: filepath.Join(dir, "smtp.password"),
	})
}

// bankTx builds one booked EUR bank transaction.
func (s *DeliverySuite) bankTx(date, amount, ref, description string) bank.Transaction {
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

// warnDeliveryLines returns every non-empty line of raw carrying a WARN-level "delivery failed"
// record, so a case can assert exactly how many per-recipient failures were logged.
func warnDeliveryLines(raw string) []string {
	var lines []string

	for line := range strings.SplitSeq(raw, "\n") {
		if strings.Contains(line, "level=WARN") && strings.Contains(line, `msg="delivery failed"`) {
			lines = append(lines, line)
		}
	}

	return lines
}
