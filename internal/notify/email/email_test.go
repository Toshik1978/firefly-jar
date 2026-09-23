package email

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
	"encoding/base64"
	"io"
	"math/big"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/textproto"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/suite"

	"github.com/Toshik1978/firefly-jar/internal/config"
	"github.com/Toshik1978/firefly-jar/internal/digest"
	"github.com/Toshik1978/firefly-jar/internal/notify"
)

// TestEmail is the single entry point for package email's test suites.
func TestEmail(t *testing.T) {
	suite.Run(t, new(EmailSuite))
}

const (
	// testHost is the configured SMTP host. The test certificate is issued for it, so a notifier
	// that verifies the server name against it passes, while the fake server itself listens on a
	// random 127.0.0.1 port that the tests put into the unexported addr field.
	testHost = "localhost"

	testUser     = "jar-sender"
	testPassword = "app-password-not-real"

	fromAddr    = "firefly-jar@example.com"
	ownerAddr   = "owner@example.com"
	partnerAddr = "partner@example.com"
	bouncedAddr = "bounced@example.com"

	portSTARTTLS = 587
	portImplicit = 465

	// sessionTimeout is generous so the happy paths never trip the deadline on a slow CI runner.
	sessionTimeout = 5 * time.Second

	// shortTimeout and deadlineBound drive the deadline cases: a silent server must fail the send
	// well inside deadlineBound once the notifier's own deadline is shortTimeout. These are real
	// sockets, so the time is real; it is kept short.
	shortTimeout  = 200 * time.Millisecond
	deadlineBound = 2 * time.Second

	// waitBound and pollEvery bound how long the tests wait for the fake server to see a
	// connection open or close.
	waitBound = 2 * time.Second
	pollEvery = 5 * time.Millisecond

	// defaultTimeout is the connection deadline New sets when nothing overrides it.
	defaultTimeout = 30 * time.Second
)

// serverMode selects how the fake SMTP server behaves on every connection it accepts.
type serverMode int

const (
	// modeSTARTTLS is a plain listener that offers STARTTLS until the session is encrypted.
	modeSTARTTLS serverMode = iota
	// modeNoSTARTTLS is a plain listener that never offers STARTTLS but still offers AUTH PLAIN,
	// tempting a careless client to send credentials in clear.
	modeNoSTARTTLS
	// modeImplicitTLS is a TLS listener (port 465 style): the session is encrypted from the
	// first byte.
	modeImplicitTLS
	// modeSilent accepts connections and reads them, but never writes a byte, not even the
	// greeting.
	modeSilent
)

// command is one line the fake server read outside DATA, and whether it arrived encrypted.
type command struct {
	line string
	tls  bool
}

func (c command) verb() string {
	fields := strings.Fields(c.line)
	if len(fields) == 0 {
		return ""
	}

	return strings.ToUpper(fields[0])
}

// message is one accepted DATA payload: the raw bytes between DATA and the terminating dot, with
// dot-stuffing undone and line endings untouched, plus the recipients accepted for it.
type message struct {
	rcpts []string
	raw   []byte
}

// session is everything the fake server saw on one connection.
type session struct {
	authUser string
	authPass string
	commands []command
	pending  []string
	messages []message
	authed   bool
}

// fakeSMTP is an in-test SMTP server on 127.0.0.1. Every field below mu is guarded by it: the
// race detector does not treat socket I/O as synchronisation, so the tests read what the server
// saw only through snapshot.
type fakeSMTP struct {
	listener  net.Listener
	serverTLS *tls.Config
	reject    string
	// rejectReply, when set, is the reply to the RCPT of reject instead of a plain 550.
	rejectReply string
	// authReply, when set, refuses every AUTH with this reply.
	authReply string
	remotes   []string
	conns     []net.Conn
	sessions  []*session
	mode      serverMode
	active    int
	mu        sync.Mutex
}

func (f *fakeSMTP) addr() string {
	return f.listener.Addr().String()
}

func (f *fakeSMTP) acceptLoop() {
	for {
		conn, err := f.listener.Accept()
		if err != nil {
			return
		}

		f.mu.Lock()
		f.remotes = append(f.remotes, conn.RemoteAddr().String())
		f.conns = append(f.conns, conn)
		f.active++
		f.mu.Unlock()

		go f.serve(conn)
	}
}

func (f *fakeSMTP) close() {
	_ = f.listener.Close()

	f.mu.Lock()
	defer f.mu.Unlock()

	for _, c := range f.conns {
		_ = c.Close()
	}
}

func (f *fakeSMTP) serve(conn net.Conn) {
	defer f.finish()
	defer conn.Close()

	if f.mode == modeSilent {
		_, _ = io.Copy(io.Discard, conn)
		return
	}

	_, encrypted := conn.(*tls.Conn)
	st := &connState{fake: f, sess: f.newSession(), conn: conn, text: textproto.NewConn(conn), tls: encrypted}

	if !st.reply("220 " + testHost + " ESMTP fake") {
		return
	}

	for {
		line, err := st.text.ReadLine()
		if err != nil {
			return
		}

		f.record(st.sess, command{line: line, tls: st.tls})

		if !st.dispatch(line) {
			return
		}
	}
}

func (f *fakeSMTP) finish() {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.active--
}

func (f *fakeSMTP) idle() bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.active == 0
}

func (f *fakeSMTP) newSession() *session {
	f.mu.Lock()
	defer f.mu.Unlock()

	sess := new(session)
	f.sessions = append(f.sessions, sess)

	return sess
}

func (f *fakeSMTP) record(sess *session, c command) {
	f.update(func() { sess.commands = append(sess.commands, c) })
}

func (f *fakeSMTP) update(change func()) {
	f.mu.Lock()
	defer f.mu.Unlock()

	change()
}

// snapshot deep-copies every session so the tests can read them without holding mu.
func (f *fakeSMTP) snapshot() []session {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]session, 0, len(f.sessions))

	for _, sess := range f.sessions {
		msgs := make([]message, 0, len(sess.messages))
		for _, m := range sess.messages {
			msgs = append(msgs, message{rcpts: slices.Clone(m.rcpts), raw: bytes.Clone(m.raw)})
		}

		out = append(out, session{
			authUser: sess.authUser,
			authPass: sess.authPass,
			commands: slices.Clone(sess.commands),
			pending:  slices.Clone(sess.pending),
			messages: msgs,
			authed:   sess.authed,
		})
	}

	return out
}

// acceptedBefore reports whether remote was accepted and, if so, how many connections the
// server accepted before it.
func (f *fakeSMTP) acceptedBefore(remote string) (int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	i := slices.Index(f.remotes, remote)

	return i, i >= 0
}

// connState is one connection's protocol state. conn and text change when STARTTLS upgrades
// the session.
type connState struct {
	fake *fakeSMTP
	sess *session
	conn net.Conn
	text *textproto.Conn
	tls  bool
}

func (st *connState) reply(line string) bool {
	return st.text.PrintfLine("%s", line) == nil
}

func (st *connState) dispatch(line string) bool {
	switch (command{line: line}).verb() {
	case "EHLO", "HELO":
		return st.ehlo()
	case "STARTTLS":
		return st.startTLS()
	case "AUTH":
		return st.auth(line)
	case "MAIL", "RSET":
		st.fake.update(func() { st.sess.pending = nil })

		return st.reply("250 2.0.0 ok")
	case "RCPT":
		return st.rcpt(line)
	case "DATA":
		return st.data()
	case "NOOP":
		return st.reply("250 2.0.0 ok")
	case "QUIT":
		st.reply("221 2.0.0 bye")

		return false
	default:
		return st.reply("502 5.5.2 command not recognised")
	}
}

// ehlo always offers AUTH PLAIN, even in clear, so only the client's own policy keeps
// credentials off an unencrypted connection.
func (st *connState) ehlo() bool {
	ext := []string{testHost + " greets the client"}
	if st.fake.mode == modeSTARTTLS && !st.tls {
		ext = append(ext, "STARTTLS")
	}

	ext = append(ext, "AUTH PLAIN")

	for i, e := range ext {
		sep := "-"
		if i == len(ext)-1 {
			sep = " "
		}

		if !st.reply("250" + sep + e) {
			return false
		}
	}

	return true
}

func (st *connState) startTLS() bool {
	if st.tls || st.fake.mode != modeSTARTTLS {
		return st.reply("502 5.5.1 STARTTLS not offered")
	}

	if !st.reply("220 2.0.0 ready to start TLS") {
		return false
	}

	tlsConn := tls.Server(st.conn, st.fake.serverTLS)
	if tlsConn.HandshakeContext(context.Background()) != nil {
		return false
	}

	st.conn, st.text, st.tls = tlsConn, textproto.NewConn(tlsConn), true

	return true
}

func (st *connState) auth(line string) bool {
	if st.fake.authReply != "" {
		return st.reply(st.fake.authReply)
	}

	fields := strings.Fields(line)
	if len(fields) < 2 || !strings.EqualFold(fields[1], "PLAIN") {
		return st.reply("504 5.5.4 mechanism not supported")
	}

	var initial string
	if len(fields) > 2 {
		initial = fields[2]
	} else {
		if !st.reply("334 ") {
			return false
		}

		resp, err := st.text.ReadLine()
		if err != nil {
			return false
		}

		st.fake.record(st.sess, command{line: resp, tls: st.tls})
		initial = resp
	}

	raw, err := base64.StdEncoding.DecodeString(initial)
	parts := strings.Split(string(raw), "\x00")

	if err != nil || len(parts) != 3 {
		return st.reply("501 5.5.2 malformed credentials")
	}

	st.fake.update(func() {
		st.sess.authed = true
		st.sess.authUser = parts[1]
		st.sess.authPass = parts[2]
	})

	return st.reply("235 2.7.0 authentication successful")
}

func (st *connState) rcpt(line string) bool {
	addr := angleAddr(line)
	if addr == st.fake.reject {
		if st.fake.rejectReply != "" {
			return st.reply(st.fake.rejectReply)
		}

		return st.reply("550 5.1.1 mailbox unavailable")
	}

	st.fake.update(func() { st.sess.pending = append(st.sess.pending, addr) })

	return st.reply("250 2.1.5 recipient ok")
}

func (st *connState) data() bool {
	var pending []string

	st.fake.update(func() { pending = slices.Clone(st.sess.pending) })

	if len(pending) == 0 {
		return st.reply("554 5.5.1 no valid recipients")
	}

	if !st.reply("354 end data with <CR><LF>.<CR><LF>") {
		return false
	}

	raw, err := readData(st.text.R)
	if err != nil {
		return false
	}

	st.fake.update(func() {
		st.sess.messages = append(st.sess.messages, message{rcpts: pending, raw: raw})
		st.sess.pending = nil
	})

	return st.reply("250 2.0.0 queued")
}

// readData reads a DATA payload byte for byte up to the lone-dot line. It does not go through
// textproto's DotReader on purpose: that one turns CRLF into LF, and the tests need to see the
// line endings the client actually sent.
func readData(r *bufio.Reader) ([]byte, error) {
	var buf bytes.Buffer

	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err //nolint:wrapcheck // Test-only helper; the caller only checks for nil.
		}

		if line == ".\r\n" {
			return buf.Bytes(), nil
		}

		buf.WriteString(strings.TrimPrefix(line, "."))
	}
}

func angleAddr(line string) string {
	start, end := strings.IndexByte(line, '<'), strings.LastIndexByte(line, '>')
	if start < 0 || end < start {
		return ""
	}

	return line[start+1 : end]
}

// EmailSuite drives the email notifier against fakeSMTP over real 127.0.0.1 sockets, so nothing
// here runs inside a synctest bubble. The notifier picks STARTTLS or implicit TLS from cfg.Port
// while the fake listens on a random port, which is why these tests are internal and override
// the unexported addr and timeout fields.
type EmailSuite struct {
	suite.Suite

	roots     *x509.CertPool
	serverTLS *tls.Config
}

// SetupSuite issues one self-signed certificate for testHost and 127.0.0.1. The server presents
// it and the notifier trusts it only through the RootCAs it is given.
func (s *EmailSuite) SetupSuite() {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	s.Require().NoError(err)

	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	s.Require().NoError(err)

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: testHost},
		DNSNames:              []string{testHost},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	s.Require().NoError(err)

	leaf, err := x509.ParseCertificate(der)
	s.Require().NoError(err)

	s.roots = x509.NewCertPool()
	s.roots.AddCert(leaf)
	s.serverTLS = &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}},
		MinVersion:   tls.VersionTLS12,
	}
}

func (s *EmailSuite) TestNameIsEmail() {
	var n notify.Notifier = New(s.config(portSTARTTLS, ownerAddr), testPassword, s.clientTLS())

	s.Equal("email", n.Name())
}

func (s *EmailSuite) TestNewDefaultsToConfiguredHostPortAndThirtySeconds() {
	for _, port := range []int{portSTARTTLS, portImplicit} {
		s.Run(strconv.Itoa(port), func() {
			n := New(s.config(port, ownerAddr), testPassword, s.clientTLS())

			s.Equal(net.JoinHostPort(testHost, strconv.Itoa(port)), n.addr)
			s.Equal(defaultTimeout, n.timeout)
		})
	}
}

// TestPort587UpgradesWithSTARTTLSBeforeAuth: everything before STARTTLS is plain EHLO and the
// STARTTLS itself; AUTH and everything after it arrive encrypted.
func (s *EmailSuite) TestPort587UpgradesWithSTARTTLSBeforeAuth() {
	srv := s.startServer(modeSTARTTLS, "")
	n := s.notifier(srv, s.config(portSTARTTLS, ownerAddr), s.clientTLS())

	results := n.Send(s.T().Context(), sampleDigest())

	s.requireDelivered(results, []string{ownerAddr})

	sessions := s.waitIdle(srv)
	s.Require().Len(sessions, 1)

	sess := sessions[0]
	upgrade := slices.IndexFunc(sess.commands, func(c command) bool { return c.verb() == "STARTTLS" })
	auth := slices.IndexFunc(sess.commands, func(c command) bool { return c.verb() == "AUTH" })

	s.Require().GreaterOrEqual(upgrade, 0, "STARTTLS never sent")
	s.Require().Greater(auth, upgrade, "AUTH missing or sent before STARTTLS")

	for _, c := range sess.commands[:upgrade+1] {
		s.False(c.tls, c.verb())
		s.Contains([]string{"EHLO", "HELO", "STARTTLS"}, c.verb())
	}

	for _, c := range sess.commands[upgrade+1:] {
		s.True(c.tls, "%s sent in clear", c.verb())
	}

	s.True(sess.authed)
	s.Equal(testUser, sess.authUser)
	s.Equal(testPassword, sess.authPass)
	s.Len(sess.messages, 1)
}

// TestPort587FailsBeforeAuthWithoutTLS: when the session cannot be encrypted, every recipient
// fails and neither the credentials nor the message ever leave in clear, even though the fake
// offers AUTH PLAIN on the plain connection (and net/smtp would send it to a localhost server).
func (s *EmailSuite) TestPort587FailsBeforeAuthWithoutTLS() {
	cases := []struct {
		name     string
		roots    *x509.CertPool
		mode     serverMode
		insecure bool
	}{
		{name: "server does not offer STARTTLS", mode: modeNoSTARTTLS, roots: s.roots},
		{name: "server certificate is not trusted", mode: modeSTARTTLS, roots: x509.NewCertPool()},
		{
			name:     "caller asks to skip verification",
			mode:     modeSTARTTLS,
			roots:    x509.NewCertPool(),
			insecure: true,
		},
	}

	for _, tc := range cases {
		s.Run(tc.name, func() {
			to := []string{ownerAddr, partnerAddr}
			srv := s.startServer(tc.mode, "")
			tlsCfg := &tls.Config{RootCAs: tc.roots, MinVersion: tls.VersionTLS12, InsecureSkipVerify: tc.insecure}
			n := s.notifier(srv, s.config(portSTARTTLS, to...), tlsCfg)

			results := n.Send(s.T().Context(), sampleDigest())

			s.requireAllFailed(results, to)

			creds := base64.StdEncoding.EncodeToString([]byte("\x00" + testUser + "\x00" + testPassword))

			for _, sess := range s.waitIdle(srv) {
				s.False(sess.authed)
				s.Empty(sess.messages)

				for _, c := range sess.commands {
					s.NotContains([]string{"AUTH", "MAIL", "RCPT", "DATA"}, c.verb())
					s.NotContains(c.line, testPassword)
					s.NotContains(c.line, creds)
				}
			}
		})
	}
}

func (s *EmailSuite) TestPort587WithoutUsernameStillEncryptsAndSkipsAuth() {
	srv := s.startServer(modeSTARTTLS, "")
	cfg := s.config(portSTARTTLS, ownerAddr)
	cfg.Username = ""
	n := s.notifier(srv, cfg, s.clientTLS())

	results := n.Send(s.T().Context(), sampleDigest())

	s.requireDelivered(results, []string{ownerAddr})

	sessions := s.waitIdle(srv)
	s.Require().Len(sessions, 1)

	sess := sessions[0]
	s.False(sess.authed)
	s.Len(sess.messages, 1)

	upgrade := slices.IndexFunc(sess.commands, func(c command) bool { return c.verb() == "STARTTLS" })
	s.Require().GreaterOrEqual(upgrade, 0, "STARTTLS never sent")

	for _, c := range sess.commands {
		s.NotEqual("AUTH", c.verb())
	}

	for _, c := range sess.commands[upgrade+1:] {
		s.True(c.tls, "%s sent in clear", c.verb())
	}
}

func (s *EmailSuite) TestPort465UsesImplicitTLS() {
	srv := s.startServer(modeImplicitTLS, "")
	n := s.notifier(srv, s.config(portImplicit, ownerAddr), s.clientTLS())

	results := n.Send(s.T().Context(), sampleDigest())

	s.requireDelivered(results, []string{ownerAddr})

	sessions := s.waitIdle(srv)
	s.Require().Len(sessions, 1)

	sess := sessions[0]
	s.Require().NotEmpty(sess.commands)

	for _, c := range sess.commands {
		s.True(c.tls, "%s sent in clear", c.verb())
		s.NotEqual("STARTTLS", c.verb())
	}

	s.True(sess.authed)
	s.Equal(testUser, sess.authUser)
	s.Equal(testPassword, sess.authPass)
	s.Len(sess.messages, 1)
}

func (s *EmailSuite) TestMessageHeaders() {
	d := sampleDigest()
	msg := s.parse(s.sendOne(d))

	for _, name := range []string{
		"From", "To", "Subject", "Date", "Message-Id", "Mime-Version", "Content-Type", "Content-Transfer-Encoding",
	} {
		s.Len(msg.Header[name], 1, "header %s must appear exactly once", name)
	}

	from, err := mail.ParseAddress(msg.Header.Get("From"))
	s.Require().NoError(err)
	s.Equal(fromAddr, from.Address)

	to, err := mail.ParseAddressList(msg.Header.Get("To"))
	s.Require().NoError(err)
	s.Require().Len(to, 1)
	s.Equal(ownerAddr, to[0].Address)

	s.Equal(mime.QEncoding.Encode("utf-8", d.Subject), msg.Header.Get("Subject"))

	decoded, err := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	s.Require().NoError(err)
	s.Equal(d.Subject, decoded)

	sent, err := time.Parse(time.RFC1123Z, msg.Header.Get("Date"))
	s.Require().NoError(err)
	s.WithinDuration(time.Now(), sent, time.Minute)

	s.Regexp(`^<[^<>@\s]+@[^<>@\s]+>$`, msg.Header.Get("Message-Id"))
	s.Equal("1.0", msg.Header.Get("Mime-Version"))
	s.Equal("text/plain; charset=utf-8", msg.Header.Get("Content-Type"))
	s.Equal("quoted-printable", msg.Header.Get("Content-Transfer-Encoding"))
	s.Empty(msg.Header.Get("Bcc"))
}

// TestMessageBodyIsTheWholeDigest: the body decodes to the full digest in one message (contracts
// digest.md: never split), with every line break a hard CRLF on the wire rather than an encoded
// LF.
func (s *EmailSuite) TestMessageBodyIsTheWholeDigest() {
	d := sampleDigest()
	msg := s.parse(s.sendOne(d))

	body, err := io.ReadAll(quotedprintable.NewReader(msg.Body))
	s.Require().NoError(err)

	s.NotContains(strings.ReplaceAll(string(body), "\r\n", ""), "\n", "line break encoded instead of CRLF")
	s.Equal(d.Text(), strings.ReplaceAll(string(body), "\r\n", "\n"))
}

// TestMessageUsesCRLFAndSevenBitOnly: every line of the DATA payload, headers and body, ends in
// CRLF, and the Q-encoded subject plus the quoted-printable body leave nothing above 0x7F.
func (s *EmailSuite) TestMessageUsesCRLFAndSevenBitOnly() {
	raw := s.sendOne(sampleDigest())

	s.Require().NotEmpty(raw)
	s.True(bytes.HasSuffix(raw, []byte("\r\n")), "payload must end in CRLF")

	for i, b := range raw {
		switch b {
		case '\n':
			s.True(i > 0 && raw[i-1] == '\r', "bare LF at byte %d", i)
		case '\r':
			s.True(i+1 < len(raw) && raw[i+1] == '\n', "bare CR at byte %d", i)
		}

		s.Less(b, byte(0x80), "8-bit byte at %d", i)
	}
}

func (s *EmailSuite) TestMessageIDIsUniquePerSend() {
	srv := s.startServer(modeSTARTTLS, "")
	n := s.notifier(srv, s.config(portSTARTTLS, ownerAddr), s.clientTLS())

	s.requireDelivered(n.Send(s.T().Context(), sampleDigest()), []string{ownerAddr})
	s.requireDelivered(n.Send(s.T().Context(), sampleDigest()), []string{ownerAddr})

	msgs := delivered(s.waitIdle(srv))
	s.Require().Len(msgs, 2)

	first := s.parse(msgs[0].raw).Header.Get("Message-Id")
	second := s.parse(msgs[1].raw).Header.Get("Message-Id")

	s.NotEmpty(first)
	s.NotEmpty(second)
	s.NotEqual(first, second)
}

// TestHeaderInjectionIsRejectedWithoutConnecting: a From, Subject or To carrying CR or LF would
// let it smuggle extra headers (a Bcc here). From and Subject belong to every recipient, so they
// fail the send for all of them; a To fails only itself, and when it is the only recipient
// nothing is left to send. Either way no connection is made.
func (s *EmailSuite) TestHeaderInjectionIsRejectedWithoutConnecting() {
	const bcc = "Bcc: x@example.com"

	cases := []struct {
		name    string
		from    string
		subject string
		to      []string
	}{
		{name: "From CRLF", from: "a@example.com\r\n" + bcc, to: []string{ownerAddr, partnerAddr}},
		{name: "From LF", from: "a@example.com\n" + bcc, to: []string{ownerAddr, partnerAddr}},
		{name: "From CR", from: "a@example.com\r" + bcc, to: []string{ownerAddr, partnerAddr}},
		{name: "Subject CRLF", subject: "subject\r\n" + bcc, to: []string{ownerAddr, partnerAddr}},
		{name: "only To CRLF", to: []string{"b@example.com\r\n" + bcc}},
		{name: "only To LF", to: []string{"b@example.com\n" + bcc}},
		{name: "only To CR", to: []string{"b@example.com\r" + bcc}},
	}

	for _, tc := range cases {
		s.Run(tc.name, func() {
			srv := s.startServer(modeSTARTTLS, "")
			cfg := s.config(portSTARTTLS, tc.to...)

			if tc.from != "" {
				cfg.From = tc.from
			}

			d := sampleDigest()
			if tc.subject != "" {
				d.Subject = tc.subject
			}

			n := s.notifier(srv, cfg, s.clientTLS())

			results := n.Send(s.T().Context(), d)

			s.requireAllFailed(results, tc.to)
			s.Zero(s.connectionsBeforeProbe(srv), "the notifier connected despite the invalid header")
		})
	}
}

// TestInvalidRecipientFailsAlone (FR-028): a To that is not a valid address, or carries CR or LF,
// fails only itself. The valid recipients still get the one message, whose To header and RCPTs
// leave the invalid entry out, and the error names the entry by position without quoting it.
func (s *EmailSuite) TestInvalidRecipientFailsAlone() {
	cases := []struct {
		name   string
		bad    string
		reason string
	}{
		{
			name:   "malformed",
			bad:    "stray@example.com trailing-junk",
			reason: "email: compose message: to[0]: invalid address",
		},
		{
			name:   "CRLF",
			bad:    "stray@example.com\r\nBcc: x@example.com",
			reason: "email: compose message: to[0]: contains CR or LF",
		},
	}

	for _, tc := range cases {
		s.Run(tc.name, func() {
			to := []string{tc.bad, ownerAddr}
			srv := s.startServer(modeSTARTTLS, "")
			n := s.notifier(srv, s.config(portSTARTTLS, to...), s.clientTLS())

			results := n.Send(s.T().Context(), sampleDigest())

			s.Require().Len(results, len(to))
			s.Equal(to[0], results[0].Recipient)
			s.Equal(to[1], results[1].Recipient)
			s.Require().Error(results[0].Err)
			s.Require().NoError(results[1].Err)

			s.Equal(tc.reason, results[0].Err.Error(), "the reason names the position, never the value")

			sessions := s.waitIdle(srv)
			s.Require().Len(sessions, 1)
			s.Equal([]string{ownerAddr}, rcptAddrs(sessions[0]))

			msgs := delivered(sessions)
			s.Require().Len(msgs, 1)
			s.Equal([]string{ownerAddr}, msgs[0].rcpts)
			s.Equal([]string{ownerAddr}, s.toHeader(msgs[0].raw))
		})
	}
}

// TestRejectedRecipientFailsAlone: a To refused at RCPT gets its own failed Result, the others
// are still delivered, and no accepted message lists it as a recipient.
func (s *EmailSuite) TestRejectedRecipientFailsAlone() {
	to := []string{ownerAddr, bouncedAddr, partnerAddr}
	srv := s.startServer(modeSTARTTLS, bouncedAddr)
	n := s.notifier(srv, s.config(portSTARTTLS, to...), s.clientTLS())

	results := n.Send(s.T().Context(), sampleDigest())

	s.Require().Len(results, len(to))

	for i, r := range results {
		s.Equal(to[i], r.Recipient, "results must follow config order")
	}

	s.Require().NoError(results[0].Err)
	s.Require().Error(results[1].Err)
	s.Require().NoError(results[2].Err)

	sessions := s.waitIdle(srv)
	s.Require().Len(sessions, 1)
	s.Equal(to, rcptAddrs(sessions[0]), "every configured recipient is tried once, in order")

	msgs := delivered(sessions)
	s.Require().Len(msgs, 1, "the accepted recipients share one message")
	s.Equal([]string{ownerAddr, partnerAddr}, msgs[0].rcpts)
	s.Equal(to, s.toHeader(msgs[0].raw), "the To header still lists every configured recipient")
}

// TestAllRecipientsShareOneTransaction: research R10 sends one message in one SMTP transaction,
// with one RCPT per configured recipient and a To header that lists them all.
func (s *EmailSuite) TestAllRecipientsShareOneTransaction() {
	to := []string{ownerAddr, partnerAddr}
	srv := s.startServer(modeSTARTTLS, "")
	n := s.notifier(srv, s.config(portSTARTTLS, to...), s.clientTLS())

	s.requireDelivered(n.Send(s.T().Context(), sampleDigest()), to)

	sessions := s.waitIdle(srv)
	s.Require().Len(sessions, 1)

	var mails, datas int

	for _, c := range sessions[0].commands {
		switch c.verb() {
		case "MAIL":
			mails++
		case "DATA":
			datas++
		}
	}

	s.Equal(1, mails)
	s.Equal(1, datas)
	s.Equal(to, rcptAddrs(sessions[0]))

	msgs := delivered(sessions)
	s.Require().Len(msgs, 1)
	s.Equal(to, msgs[0].rcpts)
	s.Equal(to, s.toHeader(msgs[0].raw))
}

// TestAllRecipientsRejectedSkipsData: with every recipient refused at RCPT there is nothing to
// deliver, so the notifier never starts DATA and every recipient reports its own failure.
func (s *EmailSuite) TestAllRecipientsRejectedSkipsData() {
	to := []string{bouncedAddr}
	srv := s.startServer(modeSTARTTLS, bouncedAddr)
	n := s.notifier(srv, s.config(portSTARTTLS, to...), s.clientTLS())

	s.requireAllFailed(n.Send(s.T().Context(), sampleDigest()), to)

	sessions := s.waitIdle(srv)
	s.Require().Len(sessions, 1)
	s.Empty(sessions[0].messages)

	for _, c := range sessions[0].commands {
		s.NotEqual("DATA", c.verb())
	}
}

// TestConnectionDeadlineApplies: against a server that never says a word, the notifier's own
// deadline ends the session, whether it is waiting for the SMTP greeting (587) or for the TLS
// handshake (465).
func (s *EmailSuite) TestConnectionDeadlineApplies() {
	for _, port := range []int{portSTARTTLS, portImplicit} {
		s.Run(strconv.Itoa(port), func() {
			to := []string{ownerAddr, partnerAddr}
			srv := s.startServer(modeSilent, "")
			n := s.notifier(srv, s.config(port, to...), s.clientTLS())
			n.timeout = shortTimeout

			start := time.Now()
			results := n.Send(context.Background(), sampleDigest())
			elapsed := time.Since(start)

			s.Less(elapsed, deadlineBound)
			s.requireAllFailed(results, to)
		})
	}
}

// TestContextEndsTheSession: a cancelled context, or a context deadline sooner than the
// notifier's own timeout, ends a session stuck on a silent server, whether it waits for the SMTP
// greeting (587) or for the TLS handshake (465).
func (s *EmailSuite) TestContextEndsTheSession() {
	cases := []struct {
		name string
		ctx  func() (context.Context, context.CancelFunc)
	}{
		{name: "cancelled", ctx: func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			time.AfterFunc(shortTimeout, cancel)

			return ctx, cancel
		}},
		{name: "deadline", ctx: func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), shortTimeout)
		}},
	}

	for _, tc := range cases {
		for _, port := range []int{portSTARTTLS, portImplicit} {
			s.Run(tc.name+"/"+strconv.Itoa(port), func() {
				to := []string{ownerAddr, partnerAddr}
				srv := s.startServer(modeSilent, "")
				n := s.notifier(srv, s.config(port, to...), s.clientTLS())
				n.timeout = defaultTimeout

				ctx, cancel := tc.ctx()
				defer cancel()

				start := time.Now()
				results := n.Send(ctx, sampleDigest())

				s.Less(time.Since(start), deadlineBound)
				s.requireAllFailed(results, to)
			})
		}
	}
}

// TestErrorsNeverCarryThePasswordOrARawAddress: a server reply may echo anything; whatever it
// echoes, no Result error shows the password or a configured address unmasked.
func (s *EmailSuite) TestErrorsNeverCarryThePasswordOrARawAddress() {
	echo := " 5.7.8 " + testPassword + " <" + ownerAddr + "> <" + bouncedAddr + ">"

	cases := []struct {
		name   string
		reject string
		reply  string
		auth   bool
	}{
		{name: "AUTH refused", auth: true, reply: "535" + echo},
		{name: "RCPT refused", reject: bouncedAddr, reply: "550" + echo},
	}

	for _, tc := range cases {
		s.Run(tc.name, func() {
			to := []string{ownerAddr, bouncedAddr}
			srv := s.startServer(modeSTARTTLS, tc.reject, func(f *fakeSMTP) {
				f.rejectReply = tc.reply
				if tc.auth {
					f.authReply = tc.reply
				}
			})

			n := s.notifier(srv, s.config(portSTARTTLS, to...), s.clientTLS())

			results := n.Send(s.T().Context(), sampleDigest())

			s.Require().Len(results, len(to))
			s.Require().Error(results[1].Err)

			for _, r := range results {
				if r.Err == nil {
					continue
				}

				reason := r.Err.Error()
				s.NotContains(reason, testPassword)
				s.NotContains(reason, ownerAddr)
				s.NotContains(reason, bouncedAddr)
				s.Contains(reason, notify.MaskRecipient(r.Recipient))
			}

			s.waitIdle(srv)
		})
	}
}

// startServer starts a fakeSMTP in mode that refuses reject at RCPT (none when empty). Every
// option adjusts the fake before it accepts a connection, so no field changes under a running
// server. It is closed when the current test or subtest ends.
func (s *EmailSuite) startServer(mode serverMode, reject string, options ...func(*fakeSMTP)) *fakeSMTP {
	inner, err := new(net.ListenConfig).Listen(context.Background(), "tcp", "127.0.0.1:0")
	s.Require().NoError(err)

	ln := inner
	if mode == modeImplicitTLS {
		ln = tls.NewListener(inner, s.serverTLS)
	}

	srv := &fakeSMTP{listener: ln, serverTLS: s.serverTLS, mode: mode, reject: reject}
	for _, option := range options {
		option(srv)
	}

	s.T().Cleanup(srv.close)

	go srv.acceptLoop()

	return srv
}

// notifier builds the Notifier under test and points it at srv instead of cfg's host and port.
func (s *EmailSuite) notifier(srv *fakeSMTP, cfg config.Email, tlsCfg *tls.Config) *Notifier {
	n := New(cfg, testPassword, tlsCfg)
	n.addr = srv.addr()
	n.timeout = sessionTimeout

	return n
}

func (s *EmailSuite) config(port int, to ...string) config.Email {
	return config.Email{Host: testHost, Port: port, Username: testUser, From: fromAddr, To: to}
}

func (s *EmailSuite) clientTLS() *tls.Config {
	return &tls.Config{RootCAs: s.roots, MinVersion: tls.VersionTLS12}
}

// sendOne delivers d to ownerAddr over STARTTLS and returns the one raw DATA payload the server
// accepted.
func (s *EmailSuite) sendOne(d digest.Digest) []byte {
	srv := s.startServer(modeSTARTTLS, "")
	n := s.notifier(srv, s.config(portSTARTTLS, ownerAddr), s.clientTLS())

	s.requireDelivered(n.Send(s.T().Context(), d), []string{ownerAddr})

	msgs := delivered(s.waitIdle(srv))
	s.Require().Len(msgs, 1)
	s.Equal([]string{ownerAddr}, msgs[0].rcpts)

	return msgs[0].raw
}

func (s *EmailSuite) parse(raw []byte) *mail.Message {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	s.Require().NoError(err)

	return msg
}

// waitIdle waits until every connection the server accepted has been closed (a notifier must
// not leave one open) and returns what the server saw.
func (s *EmailSuite) waitIdle(srv *fakeSMTP) []session {
	s.Require().Eventually(srv.idle, waitBound, pollEvery, "the notifier left a connection open")

	return srv.snapshot()
}

// connectionsBeforeProbe dials srv itself and returns how many connections the server accepted
// before that probe. The listen backlog is first in, first out, so a connection the notifier
// opened, even one it dropped at once, is accepted ahead of the probe.
func (s *EmailSuite) connectionsBeforeProbe(srv *fakeSMTP) int {
	probe, err := new(net.Dialer).DialContext(context.Background(), "tcp", srv.addr())
	s.Require().NoError(err)

	defer probe.Close()

	remote := probe.LocalAddr().String()

	s.Require().Eventually(func() bool {
		_, ok := srv.acceptedBefore(remote)

		return ok
	}, waitBound, pollEvery)

	before, _ := srv.acceptedBefore(remote)

	return before
}

func (s *EmailSuite) requireDelivered(results []notify.Result, to []string) {
	s.Require().Len(results, len(to))

	for i, r := range results {
		s.Equal(to[i], r.Recipient, "results must follow config order")
		s.Require().NoError(r.Err, r.Recipient)
	}
}

func (s *EmailSuite) requireAllFailed(results []notify.Result, to []string) {
	s.Require().Len(results, len(to))

	for i, r := range results {
		s.Equal(to[i], r.Recipient, "results must follow config order")
		s.Error(r.Err, r.Recipient)
	}
}

// toHeader returns the addresses the To header of raw lists, in order.
func (s *EmailSuite) toHeader(raw []byte) []string {
	list, err := mail.ParseAddressList(s.parse(raw).Header.Get("To"))
	s.Require().NoError(err)

	addrs := make([]string, 0, len(list))
	for _, a := range list {
		addrs = append(addrs, a.Address)
	}

	return addrs
}

// rcptAddrs returns the address of every RCPT the session saw, accepted or not, in order.
func rcptAddrs(sess session) []string {
	var addrs []string

	for _, c := range sess.commands {
		if c.verb() == "RCPT" {
			addrs = append(addrs, angleAddr(c.line))
		}
	}

	return addrs
}

func delivered(sessions []session) []message {
	var msgs []message
	for _, sess := range sessions {
		msgs = append(msgs, sess.messages...)
	}

	return msgs
}

// sampleDigest is an anonymized digest that exercises the encoders: a non-ASCII subject for the
// Q-encoding, a line longer than 76 characters for quoted-printable soft breaks, a trailing space
// and an equals sign that quoted-printable must escape, and a leading dot that SMTP must stuff.
func sampleDigest() digest.Digest {
	subject := "firefly-jar: 1 missing, 0 unchecked accounts (window 2026-09-13 – 2026-09-20)"

	return digest.Digest{
		Subject: subject,
		Lines: []string{
			subject,
			"",
			"Missing (1)",
			"  Example Bank · LT12…3456",
			"    2026-09-20  -12.34 EUR  Café Zürich – coffee and a very long description that keeps going on",
			".a line that starts with a dot",
			"a line with a trailing space ",
			"a = b",
		},
	}
}
