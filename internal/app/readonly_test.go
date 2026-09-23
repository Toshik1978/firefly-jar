package app_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/stretchr/testify/suite"

	"github.com/Toshik1978/firefly-jar/internal/app"
	"github.com/Toshik1978/firefly-jar/internal/state"
)

// roRequest is one HTTP request a ReadOnlyGuaranteeSuite fake server received, captured before its
// handler runs so even a request the handler itself would reject is on record: the method, the
// path and every header, so a case can prove no Psu-* header ever reached the bank (CLAUDE.md
// Invariant 2) without relying on the handler to notice one.
type roRequest struct {
	method string
	path   string
	header http.Header
}

// roRecorder wraps a real handler and records every request it receives, so "only GET reached the
// server" is proved against the request the server itself saw, not against what a client library
// claims to have sent.
type roRecorder struct {
	handler http.Handler

	mu       sync.Mutex
	requests []roRequest
}

// newRORecorder wraps handler.
func newRORecorder(handler http.Handler) *roRecorder {
	return &roRecorder{handler: handler}
}

// ServeHTTP records req, then delegates to the wrapped handler.
func (r *roRecorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.requests = append(r.requests, roRequest{
		method: req.Method,
		path:   req.URL.Path,
		header: req.Header.Clone(),
	})
	r.mu.Unlock()

	r.handler.ServeHTTP(w, req)
}

// recorded returns a copy of every request captured so far.
func (r *roRecorder) recorded() []roRequest {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]roRequest(nil), r.requests...)
}

// roEnableBanking is a minimal Enable Banking fake serving GET /accounts/{uid}/transactions: an
// empty page normally, or a 401 for every account when fail is set, so a case can exercise the
// bank-error path without a real sleep (401 is never retried by httpx.RetryTransport).
type roEnableBanking struct {
	fail bool
}

// serveTransactions answers the one endpoint a check run calls.
func (e *roEnableBanking) serveTransactions(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if e.fail {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"UNAUTHORIZED","message":"token rejected"}`))

		return
	}

	_, _ = w.Write([]byte(`{"transactions": [], "continuation_key": null}`))
}

// ReadOnlyGuaranteeSuite is the end-to-end read-only test of T078 (SC-003, CLAUDE.md Invariants 1
// and 2). Unlike every other suite in this package, it never substitutes a fake bank.Provider: the
// Factory is app.BuildDeps unmodified, so a full `check`, an `accounts` run and a run that exercises
// the bank's error path all drive the real firefly.Client and the real enablebanking.Client against
// in-process fakes that record every request's method, path and headers. It asserts the Firefly fake
// saw only GET requests and the Enable Banking fake saw only GET requests, none to a /payments path
// and none carrying a Psu-* header, and that both recorders actually saw traffic, so "only GET" is
// never proved vacuously.
type ReadOnlyGuaranteeSuite struct {
	suite.Suite

	vilnius *time.Location
}

// SetupSuite loads the configured time zone once.
func (s *ReadOnlyGuaranteeSuite) SetupSuite() {
	loc, err := time.LoadLocation("Europe/Vilnius")
	s.Require().NoError(err)

	s.vilnius = loc
}

// TestFullCheckOnlyReachesFireflyAndTheBankWithGetAndNeverAPaymentPath covers the ordinary path: a
// full `check --stdout` against a mapped account whose bank has nothing to report, driven entirely
// through the real Firefly and Enable Banking clients.
func (s *ReadOnlyGuaranteeSuite) TestFullCheckOnlyReachesFireflyAndTheBankWithGetAndNeverAPaymentPath() {
	h := s.newHarness(false)

	code := h.run("check", "--stdout", "--config", h.configPath)

	s.Equal(0, code, h.stderr.String())
	s.Empty(h.stderr.String())

	s.assertOnlyGet(h.ff, "firefly")
	s.assertOnlyGetOrHead(h.eb, "enable banking")
	s.assertNoPaymentPathOrPsuHeader(h.eb)
}

// TestAccountsOnlyReachesFireflyWithGetAndNeverCallsTheBank covers `accounts`: it lists Firefly III
// only (app/accounts.go never makes a bank call of its own), so the Enable Banking fake must record
// nothing at all, and the Firefly fake only GET.
func (s *ReadOnlyGuaranteeSuite) TestAccountsOnlyReachesFireflyWithGetAndNeverCallsTheBank() {
	h := s.newHarness(false)

	code := h.run("accounts", "--config", h.configPath)

	s.Equal(0, code, h.stderr.String())

	s.assertOnlyGet(h.ff, "firefly")
	s.Empty(h.eb.recorded(), "accounts makes no bank-provider call of its own")
}

// TestAFailureRunStillOnlyReachesFireflyAndTheBankWithGetAndNeverAPaymentPath covers a run where the
// bank's Transactions call fails for the one mapped account (a 401, never retried, so no real
// sleep): the account is left unchecked and the run exits 2 (CLAUDE.md Invariant 3, "a partial run
// never exits 0 or 1"), yet every request either fake received is still read-only.
func (s *ReadOnlyGuaranteeSuite) TestAFailureRunStillOnlyReachesFireflyAndTheBankWithGetAndNeverAPaymentPath() {
	h := s.newHarness(true)

	code := h.run("check", "--stdout", "--config", h.configPath)

	s.Equal(2, code, "the bank's error leaves the account unchecked")

	s.assertOnlyGet(h.ff, "firefly")
	s.assertOnlyGetOrHead(h.eb, "enable banking")
	s.assertNoPaymentPathOrPsuHeader(h.eb)
}

// assertOnlyGet asserts every request the Firefly III fake saw used GET (constitution §I: the client
// "MUST issue only GET requests", CLAUDE.md Invariant 1), and that it saw at least one request, so
// the assertion is never vacuous.
func (s *ReadOnlyGuaranteeSuite) assertOnlyGet(recorder *roRecorder, label string) {
	s.T().Helper()

	reqs := recorder.recorded()
	s.NotEmptyf(reqs, "%s fake must have recorded at least one request", label)

	for _, r := range reqs {
		s.Equalf(http.MethodGet, r.method, "%s: got %s %s, want GET", label, r.method, r.path)
	}
}

// assertOnlyGetOrHead asserts every request the bank fake saw used GET or HEAD (constitution §I,
// CLAUDE.md Invariant 2: account information is read, never written), and that it saw at least one
// request, so the assertion is never vacuous.
func (s *ReadOnlyGuaranteeSuite) assertOnlyGetOrHead(recorder *roRecorder, label string) {
	s.T().Helper()

	reqs := recorder.recorded()
	s.NotEmptyf(reqs, "%s fake must have recorded at least one request", label)

	for _, r := range reqs {
		s.Truef(r.method == http.MethodGet || r.method == http.MethodHead,
			"%s: got %s %s, want GET or HEAD", label, r.method, r.path)
	}
}

// assertNoPaymentPathOrPsuHeader asserts the bank fake never saw a /payments path or a Psu-* header
// (constitution §I, CLAUDE.md Invariant 2: account-information consent only, no payment endpoint,
// no Psu-* header since the tool runs unattended).
func (s *ReadOnlyGuaranteeSuite) assertNoPaymentPathOrPsuHeader(recorder *roRecorder) {
	s.T().Helper()

	for _, r := range recorder.recorded() {
		s.NotContains(r.path, "/payments", "no payment-path request may reach the bank")

		for name := range r.header {
			s.Falsef(strings.HasPrefix(http.CanonicalHeaderKey(name), "Psu-"),
				"no Psu-* header may reach the bank, got %s", name)
		}
	}
}

// roHarness is one ReadOnlyGuaranteeSuite case's wiring: the config and state written to a temp dir,
// the real Firefly and Enable Banking fakes wrapped in recorders, and the console buffers. The
// Factory is app.BuildDeps itself, never overridden, so both clients are the real ones.
type roHarness struct {
	configPath string
	env        map[string]string
	stdout     *bytes.Buffer
	stderr     *bytes.Buffer
	ff         *roRecorder
	eb         *roRecorder
	ebURL      string
	clock      time.Time
}

// run invokes app.RunEnv with args against the harness's Env, pointing the real Enable Banking
// client at the fake through Env.EnableBankingBaseURL.
func (h *roHarness) run(args ...string) int {
	return app.RunEnv(args, app.Env{
		Stdin:                strings.NewReader(""),
		Stdout:               h.stdout,
		Stderr:               h.stderr,
		Getenv:               func(key string) string { return h.env[key] },
		Now:                  func() time.Time { return h.clock },
		EnableBankingBaseURL: h.ebURL,
		Factory:              app.BuildDeps,
	})
}

// newHarness wires one case: a config at log_level: info with one bank and its one mapped account
// (checkAccountsFile's Firefly III account #1, via writeCheckBankState), a Firefly III fake and an
// Enable Banking fake, both wrapped in recorders. failBank scripts the Enable Banking fake to answer
// every account's transactions with 401 instead of an empty page.
func (s *ReadOnlyGuaranteeSuite) newHarness(failBank bool) *roHarness {
	s.T().Helper()

	dir := s.T().TempDir()

	accounts, err := os.ReadFile(filepath.Clean(checkAccountsFile))
	s.Require().NoError(err)

	events := &eventLog{}
	ff := &fakeFirefly{events: events, accountsBody: accounts}

	ffMux := http.NewServeMux()
	ffMux.HandleFunc("GET /api/v1/accounts", ff.serveAccounts)
	ffMux.HandleFunc("GET /api/v1/accounts/{id}/transactions", ff.serveTransactions)

	ffRecorder := newRORecorder(ffMux)
	ffSrv := httptest.NewServer(ffRecorder)
	s.T().Cleanup(ffSrv.Close)

	eb := &roEnableBanking{fail: failBank}
	ebMux := http.NewServeMux()
	ebMux.HandleFunc("GET /accounts/{uid}/transactions", eb.serveTransactions)

	ebRecorder := newRORecorder(ebMux)
	ebSrv := httptest.NewServer(ebRecorder)
	s.T().Cleanup(ebSrv.Close)

	key, err := os.ReadFile(filepath.Clean(cliPrivateKeyFixture))
	s.Require().NoError(err)

	privateKeyFile := filepath.Join(dir, "enablebanking.pem")
	writeSecret(s.T(), privateKeyFile, string(key))

	stateFile := filepath.Join(dir, "state.json")
	writeCheckBankState(s.T(), stateFile, state.CurrentVersion)

	configPath := filepath.Join(dir, "config.yaml")
	writeSecret(s.T(), configPath, buildConfigYAML(configOpts{
		timezone:       "Europe/Vilnius",
		stateFile:      stateFile,
		logFile:        filepath.Join(dir, "firefly-jar.log"),
		fireflyURL:     ffSrv.URL + "/api/v1",
		privateKeyFile: privateKeyFile,
		banks: []configBank{
			{key: checkBankKey, name: "Test Bank", country: "LT", display: "Testbank"},
		},
	}))

	return &roHarness{
		configPath: configPath,
		env:        map[string]string{envFireflyToken: checkFireflyToken},
		stdout:     &bytes.Buffer{},
		stderr:     &bytes.Buffer{},
		ff:         ffRecorder,
		eb:         ebRecorder,
		ebURL:      ebSrv.URL,
		clock:      time.Date(2026, 9, 22, 10, 0, 0, 0, s.vilnius),
	}
}
