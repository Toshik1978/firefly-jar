package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
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
	"github.com/Toshik1978/firefly-jar/internal/config"
	"github.com/Toshik1978/firefly-jar/internal/state"
)

// Anonymized identifiers of the AcceptanceUS2Suite's two configured banks, their sessions and
// their accounts (T064, spec.md User Story 2). us2PastedRedirect carries the state fakeAuthorizer's
// Begin always returns (authPendingState, auth_test.go), so a scripted paste completes; the
// mismatch redirect carries a different state so a case can script Complete to refuse it.
const (
	us2BankKey      = "acceptancebank"
	us2BankName     = "Acceptance Bank"
	us2BankCountry  = "LT"
	us2BankDisplay  = "Acceptance Bank"
	us2OtherBankKey = "acceptanceotherbank"
	us2OtherName    = "Acceptance Other Bank"

	us2PastedRedirect   = "https://example.com/eb-callback?code=us2-code-000&state=" + authPendingState
	us2MismatchRedirect = "https://example.com/eb-callback?code=us2-code-000&state=us2-wrong-state"

	us2NewSessionID      = "00000000-0000-0000-0000-000000000201"
	us2PrevSessionID     = "00000000-0000-0000-0000-000000000200"
	us2OtherSessionID    = "00000000-0000-0000-0000-000000000210"
	us2OtherNewSessionID = "00000000-0000-0000-0000-000000000211"

	us2AccountUID  = "00000000-0000-0000-0000-000000000221"
	us2AccountHash = "EEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEE=.US21"
	us2AccountIBAN = "LT000000000000000221"

	us2OtherAccountUID  = "00000000-0000-0000-0000-000000000231"
	us2OtherAccountHash = "FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF=.US31"
	us2OtherAccountIBAN = "LT000000000000000231"
)

// us2FireflyAccount is one Firefly III asset account the fake accounts endpoint serves, enough to
// let a bank account with the same IBAN and currency map automatically (internal/mapping), so a
// consent scenario is never confused with an unrelated mapping problem.
type us2FireflyAccount struct {
	id       string
	iban     string
	currency string
	name     string
}

// acceptanceUS2Factory is the test app.Factory: it lets the production app.BuildDeps build the real
// dependencies (so config, state and the redactor are wired exactly as in production), then swaps
// in a fake bank.Authorizer and a fake bank.Provider, so this suite never imports enablebanking and
// never reaches the network (constitution §IV).
type acceptanceUS2Factory struct {
	authorizer *fakeAuthorizer
	provider   *fakeProvider

	mu     sync.Mutex
	inputs []app.BuildInput
}

// build is the app.Factory the harness injects.
func (f *acceptanceUS2Factory) build(ctx context.Context, in app.BuildInput) (app.Deps, error) {
	f.mu.Lock()
	f.inputs = append(f.inputs, in)
	f.mu.Unlock()

	deps, err := app.BuildDeps(ctx, in)
	if err != nil {
		return app.Deps{}, fmt.Errorf("build deps: %w", err)
	}

	deps.Authorizer = f.authorizer
	deps.Provider = f.provider

	return deps, nil
}

// acceptanceUS2Harness is one US2 acceptance case's wiring: a temp dir holding the config, the
// state and the private key fixture; the environment map Env.Getenv reads; the fake Firefly III;
// the factory; the fixed clock; and the two console streams, reset before every run so a case can
// chain several commands (auth, then check) and read each command's own output in turn.
type acceptanceUS2Harness struct {
	configPath string
	statePath  string
	env        map[string]string
	firefly    *fakeFirefly
	factory    *acceptanceUS2Factory
	stdout     *bytes.Buffer
	stderr     *bytes.Buffer
	clock      time.Time
}

// run resets the console buffers, then invokes app.RunEnv with args and stdin over the harness's
// Env, so each chained command in a case is asserted against only its own output.
func (h *acceptanceUS2Harness) run(stdin string, args ...string) int {
	h.stdout.Reset()
	h.stderr.Reset()

	return app.RunEnv(args, app.Env{
		Stdin:     strings.NewReader(stdin),
		Stdout:    h.stdout,
		Stderr:    h.stderr,
		Getenv:    func(key string) string { return h.env[key] },
		Now:       func() time.Time { return h.clock },
		Transport: h.firefly.srv.Client().Transport,
		Factory:   h.factory.build,
	})
}

// AcceptanceUS2Suite is the US2 acceptance test (T064, spec.md User Story 2, tasks.md Phase 4): it
// drives the real CLI path app.RunEnv end to end, chaining `auth` and `check` exactly as the
// Independent Test describes, over a fixed clock (2026-09-22 10:00 Europe/Vilnius, a 7-day
// consent_warn_days unless a case says otherwise). It reuses auth_test.go's fakeAuthorizer,
// check_test.go's fakeProvider and fakeFirefly, and consent_test.go's Firefly III account fixture
// shape, so a defect this test finds is a defect in check.go, consent.go or auth.go, never in a
// test-only fake.
type AcceptanceUS2Suite struct {
	suite.Suite

	vilnius *time.Location
}

// SetupSuite loads the configured time zone once.
func (s *AcceptanceUS2Suite) SetupSuite() {
	loc, err := time.LoadLocation("Europe/Vilnius")
	s.Require().NoError(err)

	s.vilnius = loc
}

// TestScenario1RunningAuthPrintsALoginLinkAndReadsThePastedRedirect encodes spec.md US2 acceptance
// scenario 1: given a bank configured by name and country, running the authorization command
// prints a login link and waits for the pasted redirect address — proven here by the link
// appearing on stdout before Complete is ever called, and by Complete receiving exactly the
// address this case pastes on stdin.
func (s *AcceptanceUS2Suite) TestScenario1RunningAuthPrintsALoginLinkAndReadsThePastedRedirect() {
	h := s.newHarness(map[string]config.Bank{
		us2BankKey: {Name: us2BankName, Country: us2BankCountry, Display: us2BankDisplay},
	}, nil)
	h.factory.authorizer.session = s.newSession(us2NewSessionID, us2AccountUID, us2AccountHash, us2AccountIBAN)

	code := h.run(us2PastedRedirect+"\n", "auth", us2BankKey, "--config", h.configPath)

	s.Equal(0, code)
	s.Contains(h.stdout.String(),
		"Open this link and log in to "+us2BankName+" ("+us2BankCountry+"):\n  "+authPendingURL)
	s.Contains(h.stdout.String(),
		"After login your browser is redirected. Paste the full address from the address bar:")
	s.Contains(h.stdout.String(), "Connected "+us2BankName+" ("+us2BankCountry+"):")

	begins := h.factory.authorizer.beginRecorded()
	s.Require().Len(begins, 1)
	s.Equal(us2BankName, begins[0].Name)
	s.Equal(us2BankCountry, begins[0].Country)

	completes := h.factory.authorizer.completeRecorded()
	s.Require().Len(completes, 1, "the tool must have read the pasted redirect to reach Complete at all")
	s.Equal(us2PastedRedirect, completes[0].pasted, "exactly the pasted address is forwarded, unaltered")
}

// TestScenario2AStateMismatchRefusesTheRedirectAndSavesNothing encodes spec.md US2 acceptance
// scenario 2: a pasted redirect whose anti-forgery value does not match the one issued is refused,
// and nothing is saved.
func (s *AcceptanceUS2Suite) TestScenario2AStateMismatchRefusesTheRedirectAndSavesNothing() {
	h := s.newHarness(map[string]config.Bank{
		us2BankKey: {Name: us2BankName, Country: us2BankCountry, Display: us2BankDisplay},
	}, nil)
	h.factory.authorizer.completeErr = &bank.Error{Kind: bank.ErrStateMismatch, Detail: "redirect state mismatch"}

	_, statErr := os.Stat(h.statePath)
	s.Require().ErrorIs(statErr, fs.ErrNotExist, "no session has ever been saved before this case runs")

	code := h.run(us2MismatchRedirect+"\n", "auth", us2BankKey, "--config", h.configPath)

	s.Equal(2, code)
	s.Contains(h.stdout.String(), "Open this link and log in to "+us2BankName, "the link is still printed")
	s.NotContains(h.stdout.String(), "Connected", "a refused redirect never confirms a connection")
	s.NotEmpty(h.stderr.String())

	_, statErr = os.Stat(h.statePath)
	s.Require().ErrorIs(statErr, fs.ErrNotExist, "a refused redirect saves nothing")
	s.Empty(h.factory.authorizer.revokeRecorded(), "nothing new was saved, so nothing old is revoked")
}

// TestScenario3ASuccessfulAuthSavesOwnerOnlyStateAndReplacesOnlyThatBanksSession encodes spec.md
// US2 acceptance scenario 3: a successful authorization is saved readable only by the owner's
// system user, and replaces any previous connection for that bank while leaving every other bank's
// connection untouched.
func (s *AcceptanceUS2Suite) TestScenario3ASuccessfulAuthSavesOwnerOnlyStateAndReplacesOnlyThatBanksSession() {
	h := s.newHarness(map[string]config.Bank{
		us2BankKey:      {Name: us2BankName, Country: us2BankCountry, Display: us2BankDisplay},
		us2OtherBankKey: {Name: us2OtherName, Country: "LT", Display: us2OtherName},
	}, nil)
	h.factory.authorizer.session = s.newSession(us2NewSessionID, us2AccountUID, us2AccountHash, us2AccountIBAN)

	otherSession := s.newSession(us2OtherSessionID, us2OtherAccountUID, us2OtherAccountHash, us2OtherAccountIBAN)
	prevSession := s.newSession(us2PrevSessionID, us2AccountUID, us2AccountHash, us2AccountIBAN)
	s.Require().NoError(state.Save(h.statePath, &state.State{
		Version: state.CurrentVersion,
		Sessions: map[string]state.Session{
			us2BankKey:      prevSession,
			us2OtherBankKey: otherSession,
		},
	}))

	code := h.run(us2PastedRedirect+"\n", "auth", us2BankKey, "--config", h.configPath)

	s.Equal(0, code)

	info, err := os.Stat(h.statePath)
	s.Require().NoError(err)
	s.Equal(os.FileMode(0o600), info.Mode().Perm(), "the saved state is readable only by the owner")

	st, _, err := state.Load(h.statePath)
	s.Require().NoError(err)
	s.Require().Contains(st.Sessions, us2BankKey)
	s.Equal(us2NewSessionID, st.Sessions[us2BankKey].SessionID,
		"the previous connection for that bank is replaced")
	s.Require().Contains(st.Sessions, us2OtherBankKey)
	s.Equal(us2OtherSessionID, st.Sessions[us2OtherBankKey].SessionID,
		"every other bank's connection is untouched")
}

// TestScenario4AConsentExpiringSoonWarnsInTheDigestWithExitUnchanged encodes spec.md US2
// acceptance scenario 4 and its Independent Test ("simulate a consent that expires within the
// warning period"): a bank is authorized for real through `auth`, its saved consent is then edited
// to expire in 5 days (consent_warn_days is 7), and a `check` against that saved state warns by
// name with the re-authorize command, even though nothing is missing, without changing the exit
// code.
func (s *AcceptanceUS2Suite) TestScenario4AConsentExpiringSoonWarnsInTheDigestWithExitUnchanged() {
	h := s.newHarness(
		map[string]config.Bank{us2BankKey: {Name: us2BankName, Country: us2BankCountry, Display: us2BankDisplay}},
		[]us2FireflyAccount{{id: "1", iban: us2AccountIBAN, currency: "EUR", name: "Main Account"}},
	)
	h.factory.authorizer.session = s.newSession(us2NewSessionID, us2AccountUID, us2AccountHash, us2AccountIBAN)

	code := h.run(us2PastedRedirect+"\n", "auth", us2BankKey, "--config", h.configPath)
	s.Require().Equal(0, code, "the session must be saved for real before this case simulates its expiry")

	s.mutateSession(h, us2BankKey, func(session *state.Session) {
		session.ValidUntil = h.clock.Add(5 * 24 * time.Hour)
	})

	h.factory.provider.txs[us2AccountUID] = nil

	code = h.run("", "check", "--stdout", "--config", h.configPath)

	s.Equal(0, code, "a consent warning alone must not change the exit code")
	s.Contains(h.stdout.String(), "⏰ Consent")
	s.Contains(h.stdout.String(),
		"- "+us2BankKey+": consent expires 2026-09-27 (5 days) — run: firefly-jar auth "+us2BankKey)
	s.Empty(h.stderr.String(), "a warning that changes nothing else stays quiet on stderr")

	calls := h.factory.provider.recorded()
	s.Require().Len(calls, 1, "an expiring consent still gets its account checked")
	s.Equal(us2AccountUID, calls[0].account.UID)
}

// TestScenario5AnExpiredConsentUnchecksThatBanksAccountsWhileOthersStayCheckedAndExitIsCheckFailed
// encodes spec.md US2 acceptance scenario 5: two banks are authorized for real through `auth`, one
// bank's saved consent is then edited to have already expired, and a `check` against that saved
// state lists that bank's accounts as unchecked with reason "consent expired", still checks the
// other bank in full, and ends the run with the "check failed" outcome (exit 2).
func (s *AcceptanceUS2Suite) TestScenario5AnExpiredConsentUnchecksThatBanksAccountsWhileOthersStayCheckedAndExitIsCheckFailed() {
	h := s.newHarness(
		map[string]config.Bank{
			us2BankKey:      {Name: us2BankName, Country: us2BankCountry, Display: us2BankDisplay},
			us2OtherBankKey: {Name: us2OtherName, Country: "LT", Display: us2OtherName},
		},
		[]us2FireflyAccount{
			{id: "1", iban: us2AccountIBAN, currency: "EUR", name: "Main Account"},
			{id: "2", iban: us2OtherAccountIBAN, currency: "EUR", name: "Other Account"},
		},
	)

	h.factory.authorizer.session = s.newSession(us2NewSessionID, us2AccountUID, us2AccountHash, us2AccountIBAN)
	code := h.run(us2PastedRedirect+"\n", "auth", us2BankKey, "--config", h.configPath)
	s.Require().Equal(0, code)

	h.factory.authorizer.session = s.newSession(
		us2OtherNewSessionID, us2OtherAccountUID, us2OtherAccountHash, us2OtherAccountIBAN,
	)
	code = h.run(us2PastedRedirect+"\n", "auth", us2OtherBankKey, "--config", h.configPath)
	s.Require().Equal(0, code)

	s.mutateSession(h, us2BankKey, func(session *state.Session) {
		session.ValidUntil = h.clock // now >= ValidUntil is already EXPIRED.
	})

	h.factory.provider.txs[us2OtherAccountUID] = nil

	code = h.run("", "check", "--stdout", "--config", h.configPath)

	s.Equal(2, code, "an expired consent ends the run with the check-failed outcome")
	s.Contains(h.stdout.String(), "- "+us2BankKey+" LT00…0221 (Main): unchecked — consent expired")
	s.NotContains(h.stdout.String(), us2OtherBankKey+" LT00…0231",
		"the other bank's account must never be reported as unchecked")

	calls := h.factory.provider.recorded()
	s.Require().Len(calls, 1, "the expired bank costs no bank call, once its expiry is seen")
	s.Equal(us2OtherAccountUID, calls[0].account.UID, "the other bank is still checked in full")
}

// newHarness writes a config naming banks and a fake Firefly III serving ffAccounts into a fresh
// temp dir, with no session saved yet and no notify: channel configured, since every check in this
// suite runs --stdout (contracts/config.md: "check --stdout needs no notifier secret or
// recipient").
func (s *AcceptanceUS2Suite) newHarness(
	banks map[string]config.Bank, ffAccounts []us2FireflyAccount,
) *acceptanceUS2Harness {
	s.T().Helper()

	events := &eventLog{}
	ff := &fakeFirefly{events: events, accountsBody: s.accountsBody(ffAccounts...)}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/accounts", ff.serveAccounts)
	mux.HandleFunc("GET /api/v1/accounts/{id}/transactions", ff.serveTransactions)

	ff.srv = httptest.NewServer(mux)
	s.T().Cleanup(ff.srv.Close)

	dir := s.T().TempDir()
	statePath := filepath.Join(dir, "state.json")
	configPath := filepath.Join(dir, "config.yaml")

	key, err := os.ReadFile(filepath.Clean(cliPrivateKeyFixture))
	s.Require().NoError(err)
	s.Require().NoError(os.WriteFile(filepath.Join(dir, "enablebanking.pem"), key, cliSecretFileMode))
	s.Require().NoError(os.WriteFile(configPath, []byte(s.configYAML(dir, ff.srv.URL, banks)), cliSecretFileMode))

	return &acceptanceUS2Harness{
		configPath: configPath,
		statePath:  statePath,
		env:        map[string]string{envFireflyToken: checkFireflyToken},
		firefly:    ff,
		factory: &acceptanceUS2Factory{
			authorizer: &fakeAuthorizer{statePath: statePath},
			provider:   &fakeProvider{events: events, txs: map[string][]bank.Transaction{}},
		},
		stdout: &bytes.Buffer{},
		stderr: &bytes.Buffer{},
		clock:  time.Date(2026, 9, 22, 10, 0, 0, 0, s.vilnius),
	}
}

// configYAML renders the case's config: the given banks, a Firefly III URL and consent_warn_days
// 7, with no notify: section at all (every check in this suite runs --stdout).
func (*AcceptanceUS2Suite) configYAML(dir, fireflyURL string, banks map[string]config.Bank) string {
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

	for _, key := range sortedBankKeys(banks) {
		bk := banks[key]
		b.WriteString("  " + key + ": { name: " + bk.Name + ", country: " + bk.Country + ", display: " +
			bk.Display + " }\n")
	}

	return b.String()
}

// sortedBankKeys returns banks's keys sorted, so the rendered config is deterministic.
func sortedBankKeys(banks map[string]config.Bank) []string {
	keys := make([]string, 0, len(banks))
	for key := range banks {
		keys = append(keys, key)
	}

	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}

	return keys
}

// newSession builds one state.Session far from expiry, covering one EUR account named "Main".
func (*AcceptanceUS2Suite) newSession(sessionID, uid, hash, iban string) state.Session {
	return state.Session{
		Provider:     "enablebanking",
		SessionID:    sessionID,
		ValidUntil:   time.Date(2027, 3, 21, 10, 15, 0, 0, time.UTC),
		AuthorizedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		Accounts: []state.Account{{
			UID:      uid,
			Hash:     hash,
			IBAN:     iban,
			Currency: "EUR",
			Name:     "Main",
		}},
	}
}

// mutateSession loads h's state file, applies mutate to bankKey's saved session and saves it back,
// simulating the passage of time between `auth` and `check` the way spec.md's Independent Test asks
// for ("simulate a consent that expires...").
func (s *AcceptanceUS2Suite) mutateSession(h *acceptanceUS2Harness, bankKey string, mutate func(*state.Session)) {
	s.T().Helper()

	st, _, err := state.Load(h.statePath)
	s.Require().NoError(err)
	s.Require().Contains(st.Sessions, bankKey)

	session := st.Sessions[bankKey]
	mutate(&session)
	st.Sessions[bankKey] = session

	s.Require().NoError(state.Save(h.statePath, st))
}

// accountsBody renders accounts as one Firefly III GET /accounts page, the same shape
// testdata/firefly/accounts_nometa.json uses.
func (s *AcceptanceUS2Suite) accountsBody(accounts ...us2FireflyAccount) []byte {
	s.T().Helper()

	data := make([]map[string]any, 0, len(accounts))

	for _, a := range accounts {
		data = append(data, map[string]any{
			"type": "accounts",
			"id":   a.id,
			"attributes": map[string]any{
				"active":                  true,
				"name":                    a.name,
				"type":                    "asset",
				"account_role":            "defaultAsset",
				"currency_code":           a.currency,
				"currency_decimal_places": 2,
				"iban":                    a.iban,
			},
		})
	}

	body, err := json.Marshal(map[string]any{"data": data})
	s.Require().NoError(err)

	return body
}
