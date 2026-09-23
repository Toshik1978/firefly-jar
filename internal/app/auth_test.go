package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// Anonymized identifiers of the two configured banks, their sessions and their accounts. authBankKey
// is the bank the case authorizes; authOtherBankKey already has a session that must survive
// untouched (contracts/state.md: "auth <bank> replaces only that bank's entry").
const (
	authBankKey      = "swedbank"
	authBankName     = "Swedbank"
	authBankCountry  = "LT"
	authBankDisplay  = "Swedbank"
	authOtherBankKey = "revolut"
	authOtherName    = "Revolut"
	authUnknownBank  = "unknownbank"

	authOldSessionID   = "00000000-0000-0000-0000-0000000000cc"
	authNewSessionID   = "00000000-0000-0000-0000-0000000000dd"
	authOtherSessionID = "00000000-0000-0000-0000-0000000000ee"

	authPendingURL     = "https://auth.enablebanking.com/ais/start?sessionid=test-session-000"
	authPendingState   = "test-state-000"
	authPastedRedirect = "https://example.com/eb-callback?code=test-code-000&state=test-state-000"

	// authPromptTail is the prompt auth prints right before it reads the pasted redirect, with NO
	// trailing newline (contracts/cli.md): a real terminal's own echo of what the owner pastes, plus
	// the Enter that submits it, completes that line. A scripted stdin.Reader never echoes anything,
	// so the buffered stdout this suite captures reads the prompt and the "Connected …" line as one
	// run-on line: "> Connected …".
	authPromptTail = "> "
)

// authOpenLines is the part of stdout auth prints before it reads Env.Stdin (contracts/cli.md),
// pinned here for T061: the heading names the bank's config Name and Country, never its map key or
// its digest Display name.
const authOpenLines = "Open this link and log in to " + authBankName + " (" + authBankCountry + "):\n" +
	"  " + authPendingURL + "\n" +
	"After login your browser is redirected. Paste the full address from the address bar:\n" +
	authPromptTail

// authSuccessLine is the confirmation line contracts/cli.md shows once Complete returns: the
// account count comes from the returned session's Accounts, and the date is its ValidUntil.
const authSuccessLine = "Connected " + authBankName + " (" + authBankCountry + "): 2 accounts, consent valid until 2027-03-21.\n"

// authSuccessStdout is the full stdout of a successful run.
const authSuccessStdout = authOpenLines + authSuccessLine

// completeCall is one bank.Authorizer.Complete call the fake authorizer received.
type completeCall struct {
	bank    config.Bank
	pending bank.PendingConsent
	pasted  string
}

// revokeCall is one bank.Authorizer.Revoke call the fake authorizer received, together with the
// state file's bytes at the moment it ran, so a case can prove the save already happened.
type revokeCall struct {
	sessionID   string
	stateAtCall []byte
}

// fakeAuthorizer is a bank.Authorizer that records every call and returns scripted results, so
// package app never imports enablebanking in its own tests (constitution §IV).
type fakeAuthorizer struct {
	statePath string

	session state.Session

	beginErr    error
	completeErr error
	revokeErr   error

	mu            sync.Mutex
	beginCalls    []config.Bank
	completeCalls []completeCall
	revokeCalls   []revokeCall
}

// Begin records b and returns the scripted pending consent, or beginErr.
func (f *fakeAuthorizer) Begin(_ context.Context, b config.Bank) (bank.PendingConsent, error) {
	f.mu.Lock()
	f.beginCalls = append(f.beginCalls, b)
	f.mu.Unlock()

	if f.beginErr != nil {
		return bank.PendingConsent{}, f.beginErr
	}

	return bank.PendingConsent{URL: authPendingURL, State: authPendingState}, nil
}

// Complete records the call and returns the scripted session, or completeErr.
func (f *fakeAuthorizer) Complete(
	_ context.Context, b config.Bank, p bank.PendingConsent, pastedRedirect string,
) (state.Session, error) {
	f.mu.Lock()
	f.completeCalls = append(f.completeCalls, completeCall{bank: b, pending: p, pasted: pastedRedirect})
	f.mu.Unlock()

	if f.completeErr != nil {
		return state.Session{}, f.completeErr
	}

	return f.session, nil
}

// Revoke reads the state file at the instant it is called, so a case can assert that the new
// session was already saved to disk before this ran, then records the call and returns revokeErr.
func (f *fakeAuthorizer) Revoke(_ context.Context, sessionID string) error {
	data, _ := os.ReadFile(filepath.Clean(f.statePath))

	f.mu.Lock()
	f.revokeCalls = append(f.revokeCalls, revokeCall{sessionID: sessionID, stateAtCall: data})
	f.mu.Unlock()

	return f.revokeErr
}

// beginRecorded returns a copy of every Begin call so far.
func (f *fakeAuthorizer) beginRecorded() []config.Bank {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]config.Bank(nil), f.beginCalls...)
}

// completeRecorded returns a copy of every Complete call so far.
func (f *fakeAuthorizer) completeRecorded() []completeCall {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]completeCall(nil), f.completeCalls...)
}

// revokeRecorded returns a copy of every Revoke call so far.
func (f *fakeAuthorizer) revokeRecorded() []revokeCall {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]revokeCall(nil), f.revokeCalls...)
}

// authFactory is the test app.Factory: it records the BuildInput it was given, lets the production
// app.BuildDeps build the real dependencies (so config, state and the redactor are wired exactly as
// in production), then swaps in the fake bank.Authorizer before handing the deps back.
type authFactory struct {
	authorizer *fakeAuthorizer

	mu     sync.Mutex
	inputs []app.BuildInput
}

// build is the app.Factory the harness injects.
func (f *authFactory) build(ctx context.Context, in app.BuildInput) (app.Deps, error) {
	f.mu.Lock()
	f.inputs = append(f.inputs, in)
	f.mu.Unlock()

	deps, err := app.BuildDeps(ctx, in)
	if err != nil {
		return app.Deps{}, fmt.Errorf("build deps: %w", err)
	}

	deps.Authorizer = f.authorizer

	return deps, nil
}

// calls returns a copy of every BuildInput received so far.
func (f *authFactory) calls() []app.BuildInput {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]app.BuildInput(nil), f.inputs...)
}

// authHarness is one `auth` case: a temp dir holding the config, the state and the private key
// fixture; the environment map Env.Getenv reads; the factory; the scripted stdin; and the two
// console streams.
type authHarness struct {
	configPath string
	statePath  string
	env        map[string]string
	factory    *authFactory
	stdout     *bytes.Buffer
	stderr     *bytes.Buffer
	clock      time.Time
	stdin      string
}

// run invokes app.RunEnv with args and the harness's Env.
func (h *authHarness) run(args ...string) int {
	return app.RunEnv(args, app.Env{
		Stdin:   strings.NewReader(h.stdin),
		Stdout:  h.stdout,
		Stderr:  h.stderr,
		Getenv:  func(key string) string { return h.env[key] },
		Now:     func() time.Time { return h.clock },
		Factory: h.factory.build,
	})
}

// AuthCommandSuite covers `auth <bank>` (T060, FR-018, FR-019, contracts/cli.md, contracts/state.md):
// the login prompt, reading the pasted redirect from Env.Stdin, saving the new session, preserving
// every other bank's session, and revoking the bank's previous session only after the new one is
// durably on disk. It is the RED half of T061: app.Deps carries no Authorizer field yet, so this
// package does not compile until T061 adds bank.Authorizer to it. A fake bank.Authorizer is injected
// through the Factory, so this suite never imports enablebanking (constitution §IV).
//
// Every config is YAML written into the case's temp dir; the private key is the same anonymized
// fixture cli_test.go uses. The initial state file always carries a session for authOtherBankKey (to
// prove it is left alone) and, in most cases, a previous session for authBankKey too (to exercise
// the post-save revoke); the first-auth case omits it to prove Revoke is then never called.
type AuthCommandSuite struct {
	suite.Suite
}

// TestSuccessSavesSessionAndRevokesThePreviousOneAfterSaving covers the happy path: stdout matches
// contracts/cli.md exactly, the state file is 0600 and carries the new session's hash, iban,
// currency and name, the other bank's session is untouched, and the bank's previous session is
// revoked only once the new one is already readable back from disk.
func (s *AuthCommandSuite) TestSuccessSavesSessionAndRevokesThePreviousOneAfterSaving() {
	h := s.newHarness()

	code := h.run("auth", authBankKey, "--config", h.configPath)

	s.Equal(0, code)
	s.Equal(authSuccessStdout, h.stdout.String())
	s.Empty(h.stderr.String())

	info, err := os.Stat(filepath.Clean(h.statePath))
	s.Require().NoError(err)
	s.Equal(os.FileMode(0o600), info.Mode().Perm())

	st, _, err := state.Load(h.statePath)
	s.Require().NoError(err)
	s.Require().Contains(st.Sessions, authBankKey)
	s.requireSessionEqual(s.newSession(), st.Sessions[authBankKey])
	s.Require().Contains(st.Sessions, authOtherBankKey, "the other bank's session is preserved")
	s.requireSessionEqual(s.otherSession(), st.Sessions[authOtherBankKey])

	authorizer := h.factory.authorizer

	begins := authorizer.beginRecorded()
	s.Require().Len(begins, 1)
	s.Equal(authBankName, begins[0].Name)
	s.Equal(authBankCountry, begins[0].Country)

	completes := authorizer.completeRecorded()
	s.Require().Len(completes, 1)
	s.Equal(authPastedRedirect, completes[0].pasted)
	s.Equal(authPendingState, completes[0].pending.State)

	revokes := authorizer.revokeRecorded()
	s.Require().Len(revokes, 1, "the bank's previous session is revoked exactly once")
	s.Equal(authOldSessionID, revokes[0].sessionID)

	var onDisk state.State

	s.Require().NoError(json.Unmarshal(revokes[0].stateAtCall, &onDisk))
	s.Equal(authNewSessionID, onDisk.Sessions[authBankKey].SessionID,
		"the new session was already saved to disk when Revoke ran")
}

// TestRevokeFailureIsOnlyAWarning covers the ruling that a failed best-effort revoke of the previous
// session never fails the run: the new session is already saved, so auth still exits 0 and prints
// its success line, and the failure surfaces only as a WARN on stderr.
func (s *AuthCommandSuite) TestRevokeFailureIsOnlyAWarning() {
	h := s.newHarness()
	h.factory.authorizer.revokeErr = errors.New("revoke: connection refused")

	code := h.run("auth", authBankKey, "--config", h.configPath)

	s.Equal(0, code)
	s.Equal(authSuccessStdout, h.stdout.String())
	s.Contains(h.stderr.String(), "level=WARN")

	st, _, err := state.Load(h.statePath)
	s.Require().NoError(err)
	s.Equal(authNewSessionID, st.Sessions[authBankKey].SessionID)

	s.Len(h.factory.authorizer.revokeRecorded(), 1, "the revoke was still attempted")
}

// TestCompleteStateMismatchExitsWithoutSaving covers the failure row of contracts/cli.md: a pasted
// redirect whose state does not match the one Begin issued exits 2 and leaves the state file
// byte-identical, so a mistyped paste can never clobber an existing session.
func (s *AuthCommandSuite) TestCompleteStateMismatchExitsWithoutSaving() {
	h := s.newHarness()
	h.factory.authorizer.completeErr = &bank.Error{Kind: bank.ErrStateMismatch, Detail: "redirect state mismatch"}

	before, err := os.ReadFile(filepath.Clean(h.statePath))
	s.Require().NoError(err)

	code := h.run("auth", authBankKey, "--config", h.configPath)

	s.Equal(2, code)
	s.Contains(h.stdout.String(), "Open this link and log in to "+authBankName)
	s.NotContains(h.stdout.String(), "Connected")
	s.NotEmpty(h.stderr.String())

	after, err := os.ReadFile(filepath.Clean(h.statePath))
	s.Require().NoError(err)
	s.Equal(before, after, "nothing is saved on a state mismatch")
	s.Empty(h.factory.authorizer.revokeRecorded(), "nothing new was saved, so nothing old is revoked")
}

// TestCompleteErrorRedirectExitsWithoutSaving covers the same failure row for a redirect the bank
// sent back carrying its own error= parameter (the owner declined consent, or the bank rejected it)
// rather than a state mismatch: still exit 2, still nothing saved.
func (s *AuthCommandSuite) TestCompleteErrorRedirectExitsWithoutSaving() {
	h := s.newHarness()
	h.factory.authorizer.completeErr = fmt.Errorf(
		"parse redirect: %w", errors.New("redirect carries error=access_denied"),
	)

	before, err := os.ReadFile(filepath.Clean(h.statePath))
	s.Require().NoError(err)

	code := h.run("auth", authBankKey, "--config", h.configPath)

	s.Equal(2, code)
	s.NotContains(h.stdout.String(), "Connected")
	s.NotEmpty(h.stderr.String())

	after, err := os.ReadFile(filepath.Clean(h.statePath))
	s.Require().NoError(err)
	s.Equal(before, after, "nothing is saved on a rejected redirect")
}

// TestUnknownBankKeyExitsBeforeAnyFactoryCallOrNetwork covers contracts/cli.md: `<bank>` must be a
// key under banks: in config. An unknown key fails before the Factory is even called, so no
// Authorizer, and therefore no network client, is ever built.
func (s *AuthCommandSuite) TestUnknownBankKeyExitsBeforeAnyFactoryCallOrNetwork() {
	h := s.newHarness()

	before, err := os.ReadFile(filepath.Clean(h.statePath))
	s.Require().NoError(err)

	code := h.run("auth", authUnknownBank, "--config", h.configPath)

	s.Equal(2, code)
	s.Empty(h.stdout.String())
	s.Contains(h.stderr.String(), authUnknownBank)
	s.Empty(h.factory.calls(), "an unknown bank key builds no dependency")
	s.Empty(h.factory.authorizer.beginRecorded())

	after, err := os.ReadFile(filepath.Clean(h.statePath))
	s.Require().NoError(err)
	s.Equal(before, after)
}

// TestBeginProviderErrorExitsWithoutSaving covers the design's provider-error failure row: Begin
// itself failing (the ASPSP lookup or the consent request) prints nothing, since there is no URL yet
// to show, exits 2, and saves nothing.
func (s *AuthCommandSuite) TestBeginProviderErrorExitsWithoutSaving() {
	h := s.newHarness()
	h.factory.authorizer.beginErr = errors.New("aspsp lookup: not found")

	before, err := os.ReadFile(filepath.Clean(h.statePath))
	s.Require().NoError(err)

	code := h.run("auth", authBankKey, "--config", h.configPath)

	s.Equal(2, code)
	s.Empty(h.stdout.String())
	s.NotEmpty(h.stderr.String())
	s.Empty(h.factory.authorizer.completeRecorded())
	s.Empty(h.factory.authorizer.revokeRecorded())

	after, err := os.ReadFile(filepath.Clean(h.statePath))
	s.Require().NoError(err)
	s.Equal(before, after)
}

// TestFirstAuthWithNoPreviousSessionNeverCallsRevoke covers the first-ever auth of a bank: there is
// no previous session to replace, so Revoke is never called, and the run still exits 0 with the new
// session saved.
func (s *AuthCommandSuite) TestFirstAuthWithNoPreviousSessionNeverCallsRevoke() {
	h := s.newHarnessWithoutPriorSession()

	code := h.run("auth", authBankKey, "--config", h.configPath)

	s.Equal(0, code)
	s.Equal(authSuccessStdout, h.stdout.String())
	s.Empty(h.stderr.String())

	st, _, err := state.Load(h.statePath)
	s.Require().NoError(err)
	s.Require().Contains(st.Sessions, authBankKey)
	s.requireSessionEqual(s.newSession(), st.Sessions[authBankKey])

	s.Empty(h.factory.authorizer.revokeRecorded(), "no previous session existed, so nothing is revoked")
}

// newHarness writes a config, a state file carrying a previous session for authBankKey and one for
// authOtherBankKey, and the private key fixture into a fresh temp dir.
func (s *AuthCommandSuite) newHarness() *authHarness {
	return s.buildHarness(true)
}

// newHarnessWithoutPriorSession is the first-ever-auth case: the state file carries a session for
// authOtherBankKey only, so authBankKey has never been authorized before.
func (s *AuthCommandSuite) newHarnessWithoutPriorSession() *authHarness {
	return s.buildHarness(false)
}

// buildHarness writes a config and the private key fixture into a fresh temp dir, plus a state file
// that always carries authOtherBankKey's session and, when withPriorSession is set, authBankKey's
// previous session too.
func (s *AuthCommandSuite) buildHarness(withPriorSession bool) *authHarness {
	s.T().Helper()

	dir := s.T().TempDir()
	statePath := filepath.Join(dir, "state.json")
	configPath := filepath.Join(dir, "config.yaml")

	s.writeState(statePath, withPriorSession)

	key, err := os.ReadFile(filepath.Clean(cliPrivateKeyFixture))
	s.Require().NoError(err)
	s.Require().NoError(os.WriteFile(filepath.Join(dir, "enablebanking.pem"), key, cliSecretFileMode))
	s.Require().NoError(os.WriteFile(configPath, []byte(s.authConfigYAML(dir)), cliSecretFileMode))

	return &authHarness{
		configPath: configPath,
		statePath:  statePath,
		env:        map[string]string{},
		factory: &authFactory{authorizer: &fakeAuthorizer{
			statePath: statePath,
			session:   s.newSession(),
		}},
		stdout: &bytes.Buffer{},
		stderr: &bytes.Buffer{},
		clock:  time.Date(2026, 9, 23, 9, 0, 0, 0, time.UTC),
		stdin:  authPastedRedirect + "\n",
	}
}

// authConfigYAML renders the case's config: two banks, so a case can prove that authorizing one
// leaves the other's session untouched.
func (*AuthCommandSuite) authConfigYAML(dir string) string {
	var b strings.Builder

	b.WriteString("timezone: Europe/Vilnius\n")
	b.WriteString("window_days: 30\n")
	b.WriteString("date_tolerance_days: 3\n")
	b.WriteString("consent_warn_days: 7\n")
	b.WriteString("state_file: " + filepath.Join(dir, "state.json") + "\n")
	b.WriteString("log_file: " + filepath.Join(dir, "firefly-jar.log") + "\n")
	b.WriteString("log_level: info\n")
	b.WriteString("firefly:\n")
	b.WriteString("  url: https://firefly.example.com\n")
	b.WriteString("enablebanking:\n")
	b.WriteString("  app_id: 00000000-0000-0000-0000-000000000000\n")
	b.WriteString("  private_key_file: " + filepath.Join(dir, "enablebanking.pem") + "\n")
	b.WriteString("  redirect_url: https://example.com/eb-callback\n")
	b.WriteString("banks:\n")
	b.WriteString("  " + authBankKey + ": { name: " + authBankName + ", country: " + authBankCountry +
		", display: " + authBankDisplay + " }\n")
	b.WriteString("  " + authOtherBankKey + ": { name: " + authOtherName + ", country: LT, display: " +
		authOtherName + " }\n")

	return b.String()
}

// writeState saves a state file that always carries authOtherBankKey's session and, when
// withPriorSession is set, authBankKey's previous session too, so a case can assert either that
// authorizing the first bank replaces only its own entry, or that a first-ever auth never revokes
// anything because there was nothing to replace.
func (s *AuthCommandSuite) writeState(path string, withPriorSession bool) {
	s.T().Helper()

	sessions := map[string]state.Session{authOtherBankKey: s.otherSession()}
	if withPriorSession {
		sessions[authBankKey] = s.oldSession()
	}

	s.Require().NoError(state.Save(path, &state.State{
		Version:  state.CurrentVersion,
		Sessions: sessions,
	}))
}

// oldSession is authBankKey's previous session, present only when the case scripts one.
func (*AuthCommandSuite) oldSession() state.Session {
	return state.Session{
		Provider:     "enablebanking",
		SessionID:    authOldSessionID,
		ValidUntil:   time.Date(2026, 12, 1, 10, 0, 0, 0, time.UTC),
		AuthorizedAt: time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC),
		Accounts: []state.Account{{
			UID:      "00000000-0000-0000-0000-000000000901",
			Hash:     "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=.OLD1",
			IBAN:     "LT000000000000000901",
			Currency: "EUR",
			Name:     "Old Main",
		}},
	}
}

// otherSession is authOtherBankKey's session, present in every case, so a case can assert it never
// changes.
func (*AuthCommandSuite) otherSession() state.Session {
	return state.Session{
		Provider:     "enablebanking",
		SessionID:    authOtherSessionID,
		ValidUntil:   time.Date(2027, 1, 1, 10, 0, 0, 0, time.UTC),
		AuthorizedAt: time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC),
		Accounts: []state.Account{{
			UID:      "00000000-0000-0000-0000-000000000902",
			Hash:     "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB=.OTH1",
			IBAN:     "LT000000000000000902",
			Currency: "EUR",
			Name:     "Other Main",
		}},
	}
}

// newSession is the session the fake authorizer's Complete returns on success: two accounts, so
// stdout's "2 accounts" and the state file's account list agree.
func (*AuthCommandSuite) newSession() state.Session {
	return state.Session{
		Provider:     "enablebanking",
		SessionID:    authNewSessionID,
		ValidUntil:   time.Date(2027, 3, 21, 10, 15, 0, 0, time.UTC),
		AuthorizedAt: time.Date(2026, 9, 23, 9, 0, 0, 0, time.UTC),
		Accounts: []state.Account{
			{
				UID:      "00000000-0000-0000-0000-000000000101",
				Hash:     "CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC=.NEW1",
				IBAN:     "LT000000000000000101",
				Currency: "EUR",
				Name:     "Main",
			},
			{
				UID:      "00000000-0000-0000-0000-000000000102",
				Hash:     "DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD=.NEW2",
				IBAN:     "LT000000000000000102",
				Currency: "EUR",
				Name:     "Savings",
			},
		},
	}
}

// requireSessionEqual compares two sessions field by field: time.Time values compare by instant
// (Equal), never by struct identity, since a JSON round trip changes a time.Time's Location without
// changing what it means.
func (s *AuthCommandSuite) requireSessionEqual(want, got state.Session) {
	s.T().Helper()

	s.Equal(want.Provider, got.Provider)
	s.Equal(want.SessionID, got.SessionID)
	s.True(want.ValidUntil.Equal(got.ValidUntil), "valid_until")
	s.True(want.AuthorizedAt.Equal(got.AuthorizedAt), "authorized_at")
	s.Equal(want.Accounts, got.Accounts)
}
