package app_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
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
	"github.com/Toshik1978/firefly-jar/internal/state"
)

// Fixtures unique to PrivacySuite: a synthetic counterparty name that must never reach the file log
// or stderr, for a bank transaction with no Firefly III counterpart in the window (SC-009, FR-028).
const (
	privacyDescription = "ANON PRIVACY MART"
	privacyTxDate      = "2026-09-19"
	privacyTxAmount    = "-27.55"
	privacySMTPHost    = "127.0.0.1"
	privacySMTPPort    = 587
)

// PrivacySuite is the end-to-end privacy test of T077 (SC-009, FR-028): a full `check` against
// fixtures carrying a Firefly III token, a Telegram bot token, an SMTP password and an account
// IBAN, delivered through the real notify/telegram and notify/email notifiers pointed at
// in-process fakes (an httptest Telegram server capturing request bodies, and a hand-rolled SMTP
// server capturing DATA over a self-signed TLS certificate). It asserts no secret and no unmasked
// IBAN reaches the file log, stderr, a Telegram request body or an email DATA payload, that no
// description reaches the file log even at log_level: debug, and that a description may reach
// stderr only once every delivery has failed (FR-028's stated exception).
type PrivacySuite struct {
	suite.Suite

	vilnius *time.Location
}

// SetupSuite loads the configured time zone once.
func (s *PrivacySuite) SetupSuite() {
	loc, err := time.LoadLocation("Europe/Vilnius")
	s.Require().NoError(err)

	s.vilnius = loc
}

// TestSuccessfulDeliveryCarriesTheDescriptionButNeverASecretOrAnUnmaskedIBAN covers the ordinary
// path: both channels deliver, so the run exits 1 (missing found), stdout and stderr stay empty,
// and the fixture description reaches both delivered channels (proving the fixtures actually flowed
// through) while no secret and no unmasked IBAN reaches the file log, stderr, the Telegram request
// body or the email DATA.
func (s *PrivacySuite) TestSuccessfulDeliveryCarriesTheDescriptionButNeverASecretOrAnUnmaskedIBAN() {
	h := s.newHarness(false)

	code := h.run()

	s.Equal(1, code, "one missing transaction, delivered on both channels")
	s.Empty(h.stdout.String())
	s.Empty(h.stderr.String(), "a delivered missing-found run writes nothing to stderr")

	fileLog := s.readFile(h.logPath)
	telegramBodies := h.telegram.allBodies()
	emailData := h.smtp.allData()

	s.NotEmpty(fileLog, "the run must log at least its summary")
	s.NotEmpty(telegramBodies, "the fake Telegram server must have captured a request")
	s.NotEmpty(emailData, "the fake SMTP server must have captured a DATA payload")

	for _, secret := range []string{checkFireflyToken, cliTelegramToken, cliSMTPPassword, checkAccountIBAN} {
		s.NotContains(fileLog, secret, "a secret or unmasked IBAN must never reach the file log")
		s.NotContains(h.stderr.String(), secret, "a secret or unmasked IBAN must never reach stderr")
		s.NotContains(telegramBodies, secret, "a secret or unmasked IBAN must never reach a Telegram request body")
		s.NotContains(emailData, secret, "a secret or unmasked IBAN must never reach the email DATA")
	}

	s.NotContains(fileLog, privacyDescription, "a description must never reach the file log, even at debug")

	s.Contains(telegramBodies, privacyDescription, "the digest delivered to Telegram must name the missing transaction")
	s.Contains(emailData, privacyDescription, "the digest delivered by email must name the missing transaction")
	s.Contains(telegramBodies, checkHeading, "the masked IBAN may reach a delivered digest")
}

// TestSkippedFireflySplitIsLoggedAtDebugWithoutItsDescription covers research R8's "a split not
// comparable in the account's currency is skipped and logged at DEBUG" end to end: the record must
// reach the file log at log_level: debug (the run's logger is wired into the Firefly III client),
// carrying only group_id, account_id, date and amount, never the split's description.
func (s *PrivacySuite) TestSkippedFireflySplitIsLoggedAtDebugWithoutItsDescription() {
	const (
		skippedDescription = "ANON SKIPPED USD SPLIT"
		skipMessage        = "skip split not comparable in account currency"
	)

	h := s.newHarness(false, ffEntry{
		group: "77", kind: "withdrawal", date: privacyTxDate, amount: "27.55", currency: "USD",
		description: skippedDescription,
	})

	code := h.run()

	s.Equal(1, code, "the skipped split cannot match, so the bank transaction stays missing")

	fileLog := s.readFile(h.logPath)

	var record map[string]any

	for line := range strings.Lines(fileLog) {
		if strings.Contains(line, skipMessage) {
			s.Require().NoError(json.Unmarshal([]byte(line), &record))
		}
	}

	s.Require().NotNil(record, "the skip-split DEBUG record must reach the file log at log_level: debug")
	s.Equal("DEBUG", record["level"])
	s.Equal("77", record["group_id"])
	s.Equal(checkFireflyID, record["account_id"])
	s.Equal(privacyTxDate, record["date"])
	s.Equal("27.55", record["amount"])
	s.NotContains(fileLog, skippedDescription, "a Firefly III description must never reach the file log")
	s.NotContains(fileLog, privacyDescription, "a bank description must never reach the file log")
}

// TestEveryDeliveryFailingAllowsTheDescriptionOnStderrOnly covers FR-028's stated exception: once
// delivery has failed for every recipient across every channel, the run exits 2 and the full digest
// text, including the description, is written to stderr as a fallback so the owner still sees it.
// Even then, no secret and no unmasked IBAN may reach stderr, and the file log stays exactly as
// clean as the successful case.
func (s *PrivacySuite) TestEveryDeliveryFailingAllowsTheDescriptionOnStderrOnly() {
	h := s.newHarness(true)

	code := h.run()

	s.Equal(2, code, "delivery failed for every recipient across every channel")
	s.Empty(h.stdout.String())

	stderr := h.stderr.String()
	s.Contains(stderr, privacyDescription, "FR-028: the full digest may reach stderr once every recipient failed")

	fileLog := s.readFile(h.logPath)
	s.NotEmpty(fileLog, "the run must log at least its summary")

	for _, secret := range []string{checkFireflyToken, cliTelegramToken, cliSMTPPassword, checkAccountIBAN} {
		s.NotContains(stderr, secret, "a secret or unmasked IBAN must never reach stderr, even in the fallback")
		s.NotContains(fileLog, secret, "a secret or unmasked IBAN must never reach the file log")
	}

	s.NotContains(fileLog, privacyDescription, "the file log never carries a description, even when stderr does")
}

// newHarness wires one case: a config at log_level: debug with both notify channels, a state file
// mapping the bank's one account to Firefly III account #1 (checkAccountsFile), a fake Firefly III
// reporting entries on that account (none unless a case passes some, so the one scripted bank
// transaction is always missing), a fake Telegram server and a fake SMTP server, both scripted to
// fail every attempt when failDelivery is set.
func (s *PrivacySuite) newHarness(failDelivery bool, entries ...ffEntry) *privacyHarness {
	s.T().Helper()

	dir := s.T().TempDir()

	accounts, err := os.ReadFile(filepath.Clean(checkAccountsFile))
	s.Require().NoError(err)

	events := &eventLog{}
	ff := &fakeFirefly{events: events, accountsBody: accounts, entries: entries}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/accounts", ff.serveAccounts)
	mux.HandleFunc("GET /api/v1/accounts/{id}/transactions", ff.serveTransactions)

	ff.srv = httptest.NewServer(mux)
	s.T().Cleanup(ff.srv.Close)

	telegram := newFakeTelegramServer(s.T(), failDelivery)
	smtp := newFakeSMTPServer(s.T(), privacySMTPHost, failDelivery)

	key, err := os.ReadFile(filepath.Clean(cliPrivateKeyFixture))
	s.Require().NoError(err)
	writeSecret(s.T(), filepath.Join(dir, "enablebanking.pem"), string(key))
	writeSecret(s.T(), filepath.Join(dir, "smtp.password"), cliSMTPPassword)

	writeCheckBankState(s.T(), filepath.Join(dir, "state.json"), state.CurrentVersion)

	configPath := filepath.Join(dir, "config.yaml")
	writeSecret(s.T(), configPath, s.configYAML(dir, ff.srv.URL))

	txs := []bank.Transaction{s.bankTx(privacyTxDate, privacyTxAmount, "ref-privacy", privacyDescription)}

	return &privacyHarness{
		configPath: configPath,
		logPath:    filepath.Join(dir, "firefly-jar.log"),
		env:        map[string]string{envFireflyToken: checkFireflyToken, envTelegramToken: cliTelegramToken},
		stdout:     &bytes.Buffer{},
		stderr:     &bytes.Buffer{},
		telegram:   telegram,
		smtp:       smtp,
		factory: &privacyFactory{
			provider: &fakeProvider{events: events, txs: map[string][]bank.Transaction{checkAccountUID: txs}},
		},
		clock: time.Date(2026, 9, 22, 10, 0, 0, 0, s.vilnius),
	}
}

// configYAML renders one config at log_level: debug (T077 requires the no-description assertion at
// debug) with a Telegram and an email channel, the latter always naming the privileged port 587
// config validation allows; the harness's Env.SMTPAddr is what actually redirects the connection to
// the fake server.
func (*PrivacySuite) configYAML(dir, fireflyURL string) string {
	return buildConfigYAML(configOpts{
		timezone:          "Europe/Vilnius",
		stateFile:         filepath.Join(dir, "state.json"),
		logFile:           filepath.Join(dir, "firefly-jar.log"),
		logLevel:          "debug",
		fireflyURL:        fireflyURL,
		privateKeyFile:    filepath.Join(dir, "enablebanking.pem"),
		banks:             []configBank{{key: checkBankKey, name: "Test Bank", country: "LT", display: "Testbank"}},
		telegram:          true,
		telegramTokenFile: filepath.Join(dir, "telegram.token"),
		telegramChatIDs:   []int64{100000001},
		email:             true,
		emailPasswordFile: filepath.Join(dir, "smtp.password"),
		emailHost:         privacySMTPHost,
	})
}

// bankTx builds one EUR bank transaction carrying description as its free-text memo, which in this
// domain model doubles as the counterparty name CLAUDE.md's "descriptions and counterparty names"
// rule refers to: bank.Transaction has no separate field for it.
func (s *PrivacySuite) bankTx(date, amount, ref, description string) bank.Transaction {
	s.T().Helper()

	value, err := money.ParseAmount(amount, "EUR")
	s.Require().NoError(err)

	d, err := civil.ParseDate(date)
	s.Require().NoError(err)

	return bank.Transaction{Date: d, Amount: value, Status: bank.Booked, EntryRef: ref, Description: description}
}

// readFile reads path and requires success; the harness's own file log is always written by the
// run under test.
func (s *PrivacySuite) readFile(path string) string {
	s.T().Helper()

	raw, err := os.ReadFile(filepath.Clean(path))
	s.Require().NoError(err)

	return string(raw)
}

// privacyFactory lets the production app.BuildDeps build everything for real, including the real
// Telegram and email notifiers, then replaces only the bank provider with a fake, so no real
// Enable Banking call is ever attempted.
type privacyFactory struct {
	provider *fakeProvider
}

// build is the app.Factory the harness injects.
func (f *privacyFactory) build(ctx context.Context, in app.BuildInput) (app.Deps, error) {
	deps, err := app.BuildDeps(ctx, in)
	if err != nil {
		return app.Deps{}, fmt.Errorf("build deps: %w", err)
	}

	deps.Provider = f.provider

	return deps, nil
}

// privacyHarness is one PrivacySuite case's wiring: the config and state written to a temp dir, the
// env vars a real check run needs, the fake Telegram and SMTP servers the real notifiers are
// pointed at, the factory and the console buffers.
type privacyHarness struct {
	configPath string
	logPath    string
	env        map[string]string
	stdout     *bytes.Buffer
	stderr     *bytes.Buffer
	telegram   *fakeTelegramServer
	smtp       *fakeSMTPServer
	factory    *privacyFactory
	clock      time.Time
}

// run invokes app.RunEnv with a real check, pointing the real notifiers at the harness's fakes
// through Env.TelegramBaseURL, Env.SMTPTLSConfig and Env.SMTPAddr.
func (h *privacyHarness) run() int {
	return app.RunEnv([]string{"check", "--config", h.configPath}, app.Env{
		Stdin:           strings.NewReader(""),
		Stdout:          h.stdout,
		Stderr:          h.stderr,
		Getenv:          func(key string) string { return h.env[key] },
		Now:             func() time.Time { return h.clock },
		TelegramBaseURL: h.telegram.srv.URL,
		SMTPTLSConfig:   h.smtp.clientTLS,
		SMTPAddr:        h.smtp.addr(),
		Factory:         h.factory.build,
	})
}

// fakeTelegramServer is an in-process Bot API: it records every request body it received and,
// when fail is set, always answers ok:false so every chat id fails at once (no retry needed, since
// classify only retries a 429 or a 5xx).
type fakeTelegramServer struct {
	srv  *httptest.Server
	fail bool

	mu     sync.Mutex
	bodies [][]byte
}

// newFakeTelegramServer starts the server and registers its cleanup.
func newFakeTelegramServer(t *testing.T, fail bool) *fakeTelegramServer {
	t.Helper()

	f := &fakeTelegramServer{fail: fail}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)

	return f
}

// handle captures the request body and answers ok:true, or a 400 with ok:false when fail is set.
func (f *fakeTelegramServer) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)

	f.mu.Lock()
	f.bodies = append(f.bodies, body)
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")

	if f.fail {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"bad request"}`))

		return
	}

	_, _ = w.Write([]byte(`{"ok":true}`))
}

// allBodies joins every request body this server captured, in order.
func (f *fakeTelegramServer) allBodies() string {
	f.mu.Lock()
	defer f.mu.Unlock()

	var b strings.Builder

	for _, body := range f.bodies {
		b.Write(body)
		b.WriteByte('\n')
	}

	return b.String()
}

// smtpSession is one accepted SMTP connection: its current (possibly TLS-upgraded) conn, the
// buffered reader/writer over it, and whether STARTTLS has already run.
type smtpSession struct {
	conn      net.Conn
	rw        *bufio.ReadWriter
	tlsActive bool
}

// fakeSMTPServer is a minimal STARTTLS-capable SMTP server on 127.0.0.1: it greets, offers
// STARTTLS, accepts AUTH PLAIN unconditionally, and either records every DATA payload it received
// or, when rejectRCPT is set, refuses every RCPT so no DATA is ever sent (an all-recipients-failed
// delivery, exercised without any retry or real sleep).
type fakeSMTPServer struct {
	listener   net.Listener
	tlsCfg     *tls.Config
	clientTLS  *tls.Config
	rejectRCPT bool

	mu   sync.Mutex
	data [][]byte
}

// newFakeSMTPServer generates a self-signed certificate for host, starts listening on an ephemeral
// 127.0.0.1 port and registers its cleanup.
func newFakeSMTPServer(t *testing.T, host string, rejectRCPT bool) *fakeSMTPServer {
	t.Helper()

	serverTLS, roots := generateSMTPCert(t, host)

	listener, err := new(net.ListenConfig).Listen(t.Context(), "tcp", host+":0")
	require.NoError(t, err)

	f := &fakeSMTPServer{
		listener:   listener,
		tlsCfg:     serverTLS,
		clientTLS:  &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		rejectRCPT: rejectRCPT,
	}

	go f.acceptLoop()
	t.Cleanup(func() { _ = listener.Close() })

	return f
}

// addr is the address a client dials to reach this server.
func (f *fakeSMTPServer) addr() string {
	return f.listener.Addr().String()
}

// allData joins every DATA payload this server accepted, in order.
func (f *fakeSMTPServer) allData() string {
	f.mu.Lock()
	defer f.mu.Unlock()

	var b strings.Builder

	for _, raw := range f.data {
		b.Write(raw)
		b.WriteByte('\n')
	}

	return b.String()
}

// acceptLoop serves every connection until the listener closes.
func (f *fakeSMTPServer) acceptLoop() {
	for {
		conn, err := f.listener.Accept()
		if err != nil {
			return
		}

		go f.serve(conn)
	}
}

// serve speaks one SMTP session over conn: a greeting, then one dispatch per command line until
// QUIT or a read error.
func (f *fakeSMTPServer) serve(conn net.Conn) {
	defer conn.Close()

	sess := &smtpSession{conn: conn, rw: newSMTPConn(conn)}
	writeLine(sess.rw, "220 fake.smtp ESMTP")

	for {
		line, err := readLine(sess.rw)
		if err != nil {
			return
		}

		if !f.dispatch(sess, line) {
			return
		}
	}
}

// dispatch acts on one command line and reports whether the session continues.
func (f *fakeSMTPServer) dispatch(sess *smtpSession, line string) bool {
	upper := strings.ToUpper(line)

	switch {
	case strings.HasPrefix(upper, "EHLO"):
		f.replyEHLO(sess)
	case strings.HasPrefix(upper, "STARTTLS"):
		f.upgradeTLS(sess)
	case strings.HasPrefix(upper, "AUTH"):
		writeLine(sess.rw, "235 Authentication successful")
	case strings.HasPrefix(upper, "MAIL FROM"):
		writeLine(sess.rw, "250 OK")
	case strings.HasPrefix(upper, "RCPT TO"):
		f.replyRCPT(sess)
	case strings.HasPrefix(upper, "DATA"):
		f.receiveData(sess)
	case strings.HasPrefix(upper, "QUIT"):
		writeLine(sess.rw, "221 Bye")

		return false
	default:
		writeLine(sess.rw, "500 unrecognized command")
	}

	return true
}

// replyEHLO advertises STARTTLS before TLS and AUTH PLAIN once encrypted, matching a real server
// that never offers AUTH in clear.
func (f *fakeSMTPServer) replyEHLO(sess *smtpSession) {
	if sess.tlsActive {
		writeLines(sess.rw, []string{"250-fake.smtp", "250 AUTH PLAIN"})

		return
	}

	writeLines(sess.rw, []string{"250-fake.smtp", "250 STARTTLS"})
}

// upgradeTLS performs the STARTTLS handshake and, once it succeeds, swaps sess to the encrypted
// connection.
func (f *fakeSMTPServer) upgradeTLS(sess *smtpSession) {
	writeLine(sess.rw, "220 Ready to start TLS")

	tlsConn := tls.Server(sess.conn, f.tlsCfg)
	if err := tlsConn.HandshakeContext(context.Background()); err != nil {
		return
	}

	sess.conn = tlsConn
	sess.rw = newSMTPConn(tlsConn)
	sess.tlsActive = true
}

// replyRCPT accepts the recipient, or refuses it with a permanent failure when rejectRCPT is set,
// which fails every recipient without ever reaching DATA.
func (f *fakeSMTPServer) replyRCPT(sess *smtpSession) {
	if f.rejectRCPT {
		writeLine(sess.rw, "550 no such user")

		return
	}

	writeLine(sess.rw, "250 OK")
}

// receiveData reads the dot-terminated payload and records it, unless the transport itself broke.
func (f *fakeSMTPServer) receiveData(sess *smtpSession) {
	writeLine(sess.rw, "354 go ahead")

	raw, err := readSMTPData(sess.rw)
	if err != nil {
		return
	}

	f.mu.Lock()
	f.data = append(f.data, raw)
	f.mu.Unlock()

	writeLine(sess.rw, "250 OK: queued")
}

// generateSMTPCert issues a self-signed, CA-true certificate for host (both as CN/DNS name and as
// an IP SAN, since the notifier's TLS server name defaults to the configured host) and returns the
// server-side TLS config carrying it alongside a client-side pool that trusts it.
func generateSMTPCert(t *testing.T, host string) (*tls.Config, *x509.CertPool) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	require.NoError(t, err)

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: host},
		DNSNames:              []string{host},
		IPAddresses:           []net.IP{net.ParseIP(host)},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)

	leaf, err := x509.ParseCertificate(der)
	require.NoError(t, err)

	roots := x509.NewCertPool()
	roots.AddCert(leaf)

	serverTLS := &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}},
		MinVersion:   tls.VersionTLS12,
	}

	return serverTLS, roots
}

// newSMTPConn wraps conn in a buffered reader/writer for line-oriented SMTP I/O.
func newSMTPConn(conn net.Conn) *bufio.ReadWriter {
	return bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn))
}

// readLine reads one CRLF-terminated command or reply line, without the terminator.
func readLine(rw *bufio.ReadWriter) (string, error) {
	line, err := rw.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("read smtp line: %w", err)
	}

	return strings.TrimRight(line, "\r\n"), nil
}

// writeLine writes one CRLF-terminated reply line and flushes it.
func writeLine(rw *bufio.ReadWriter, s string) {
	_, _ = rw.WriteString(s + "\r\n")
	_ = rw.Flush()
}

// writeLines writes a multi-line reply (every line but the last already carries its own "-" or " "
// after the status code) and flushes it once.
func writeLines(rw *bufio.ReadWriter, lines []string) {
	for _, l := range lines {
		_, _ = rw.WriteString(l + "\r\n")
	}

	_ = rw.Flush()
}

// readSMTPData reads DATA's payload up to the terminating "." line, undoing dot-stuffing, and
// joins it back with CRLF.
func readSMTPData(rw *bufio.ReadWriter) ([]byte, error) {
	var lines []string

	for {
		line, err := readLine(rw)
		if err != nil {
			return nil, err
		}

		if line == "." {
			break
		}

		lines = append(lines, strings.TrimPrefix(line, "."))
	}

	return []byte(strings.Join(lines, "\r\n")), nil
}
