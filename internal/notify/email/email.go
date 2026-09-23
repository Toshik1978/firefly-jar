// Package email delivers a digest as one plain-text email over SMTP (research R10). It uses
// net/smtp directly rather than smtp.SendMail, because SendMail upgrades to TLS only when the
// server offers it and has no deadline: here port 587 requires STARTTLS and fails before AUTH
// without it, port 465 is TLS from the first byte, and one deadline covers the whole session.
package email

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"github.com/Toshik1978/firefly-jar/internal/config"
	"github.com/Toshik1978/firefly-jar/internal/digest"
	"github.com/Toshik1978/firefly-jar/internal/notify"
)

const (
	// channelName is what Name reports, and so the channel of every delivery failure.
	channelName = "email"
	// defaultDeadline bounds the whole SMTP session, from dial to QUIT.
	defaultDeadline = 30 * time.Second
	// portImplicitTLS is the submission port that speaks TLS from the first byte; every other
	// port the config allows (587) must upgrade with STARTTLS.
	portImplicitTLS = 465
	// heloName is what the notifier calls itself in EHLO; it runs from cron and has no public
	// name worth announcing.
	heloName = "localhost"
	// redacted replaces the password wherever it would otherwise reach an error.
	redacted = "<redacted>"
)

// errNoSTARTTLS fails a port 587 session whose server cannot be asked to encrypt it.
var errNoSTARTTLS = errors.New("server does not offer STARTTLS")

// Notifier sends a digest to a fixed list of addresses as one message in one SMTP transaction.
// It is safe to reuse across Send calls but holds no state between them.
type Notifier struct {
	tlsCfg      *tls.Config
	addr        string
	host        string
	username    string
	password    string
	from        string
	to          []string
	timeout     time.Duration
	implicitTLS bool
}

// New returns a Notifier for cfg that authenticates with password (AUTH PLAIN, only when
// cfg.Username is set). tlsCfg supplies the trust roots; nil means the system roots. Its server
// name defaults to cfg.Host and it never allows less than TLS 1.2.
func New(cfg config.Email, password string, tlsCfg *tls.Config) *Notifier {
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	if tlsCfg != nil {
		tc = tlsCfg.Clone()
		tc.MinVersion = max(tc.MinVersion, tls.VersionTLS12)
	}

	if tc.ServerName == "" {
		tc.ServerName = cfg.Host
	}

	// The certificate is always verified: a digest and a password must never go to whoever
	// answers on the SMTP port.
	tc.InsecureSkipVerify = false

	return &Notifier{
		tlsCfg:      tc,
		addr:        net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)),
		host:        cfg.Host,
		username:    cfg.Username,
		password:    password,
		from:        cfg.From,
		to:          append([]string(nil), cfg.To...),
		timeout:     defaultDeadline,
		implicitTLS: cfg.Port == portImplicitTLS,
	}
}

// WithAddr returns a copy of n dialing addr instead of the host and port cfg gave New. Only a test
// uses this (mirroring bank/enablebanking's Client.WithAuthConfig): config validation restricts
// notify.email.port to 587 or 465, both privileged, so it is otherwise impossible to point a
// Notifier at a fake SMTP server bound to an ordinary port.
func (n *Notifier) WithAddr(addr string) *Notifier {
	cp := *n
	cp.addr = addr

	return &cp
}

// Name reports the channel name, "email".
func (n *Notifier) Name() string {
	return channelName
}

// Send delivers d to every configured address and returns one Result per address in config
// order, with the address as configured as the Recipient. An invalid From or Subject, or a
// failure of the session, fails every address; an invalid address, or a refusal at RCPT, fails
// only that address. An error never carries the password, and every configured address in it is
// masked.
func (n *Notifier) Send(ctx context.Context, d digest.Digest) []notify.Result {
	errs := n.outcomes(ctx, d)
	results := make([]notify.Result, 0, len(n.to))

	for i, to := range n.to {
		result := notify.Result{Recipient: to}

		switch {
		case errs[i] == nil:
		case errors.Is(errs[i], errLineBreak), errors.Is(errs[i], errInvalidAddress):
			// A configuration error names its field by position; the value, even masked,
			// may be the malformed text itself.
			result.Err = n.scrub(fmt.Errorf("email: %w", errs[i]))
		default:
			result.Err = n.scrub(fmt.Errorf("email: %s: %w", notify.MaskRecipient(to), errs[i]))
		}

		results = append(results, result)
	}

	return results
}

// outcomes composes and delivers d and returns, per configured address, why it failed (nil when
// it was delivered). Nothing is dialled when no address is valid.
func (n *Notifier) outcomes(ctx context.Context, d digest.Digest) []error {
	errs := make([]error, len(n.to))

	env, err := compose(n.from, n.to, d, time.Now())
	if err != nil {
		fill(errs, fmt.Errorf("compose message: %w", err))

		return errs
	}

	for i, invalid := range env.invalid {
		if invalid != nil {
			errs[i] = fmt.Errorf("compose message: %w", invalid)
		}
	}

	if len(env.rcpts) == 0 {
		return errs
	}

	sent := make([]error, len(env.rcpts))
	n.deliver(ctx, env, sent)

	for k, slot := range env.slots {
		errs[slot] = sent[k]
	}

	return errs
}

// deliver runs one SMTP session and records its outcome in errs, one slot per recipient. A
// cancelled ctx ends the session at once by moving the connection deadline into the past.
func (n *Notifier) deliver(ctx context.Context, env *envelope, errs []error) {
	conn, err := n.dial(ctx)
	if err != nil {
		fill(errs, err)

		return
	}

	defer conn.Close()

	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	if err := n.session(conn, env, errs); err != nil {
		fill(errs, err)
	}
}

// dial connects, with TLS from the first byte on port 465, and applies the session deadline:
// the configured timeout or ctx's deadline, whichever comes first.
func (n *Notifier) dial(ctx context.Context) (net.Conn, error) {
	deadline := time.Now().Add(n.timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}

	dialCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	var (
		conn net.Conn
		err  error
	)

	if n.implicitTLS {
		conn, err = (&tls.Dialer{Config: n.tlsCfg}).DialContext(dialCtx, "tcp", n.addr)
	} else {
		conn, err = new(net.Dialer).DialContext(dialCtx, "tcp", n.addr)
	}

	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}

	if err := conn.SetDeadline(deadline); err != nil {
		_ = conn.Close()

		return nil, fmt.Errorf("set deadline: %w", err)
	}

	return conn, nil
}

// session speaks SMTP over conn: greeting, EHLO, STARTTLS (port 587) and AUTH, then one
// transaction for every recipient. A returned error fails every recipient not already failed
// at RCPT.
func (n *Notifier) session(conn net.Conn, env *envelope, errs []error) error {
	c, err := smtp.NewClient(conn, n.host)
	if err != nil {
		return fmt.Errorf("read greeting: %w", err)
	}

	defer c.Close()

	err = n.handshake(c)
	if err == nil {
		err = transact(c, env, errs)
	}

	if err != nil {
		return err
	}

	// The server has already queued the message or refused every recipient; a failed QUIT
	// changes neither.
	_ = c.Quit()

	return nil
}

// handshake gets the session ready for a transaction: encrypted and, when a username is
// configured, authenticated.
func (n *Notifier) handshake(c *smtp.Client) error {
	if err := c.Hello(heloName); err != nil {
		return fmt.Errorf("ehlo: %w", err)
	}

	if err := n.secure(c); err != nil {
		return err
	}

	return n.authenticate(c)
}

// secure upgrades a port 587 session with STARTTLS and refuses to go on in clear, so neither the
// credentials nor the digest ever leave unencrypted. On port 465 the session is already TLS.
func (n *Notifier) secure(c *smtp.Client) error {
	if n.implicitTLS {
		return nil
	}

	if ok, _ := c.Extension("STARTTLS"); !ok {
		return errNoSTARTTLS
	}

	if err := c.StartTLS(n.tlsCfg); err != nil {
		return fmt.Errorf("starttls: %w", err)
	}

	return nil
}

func (n *Notifier) authenticate(c *smtp.Client) error {
	if n.username == "" {
		return nil
	}

	if err := c.Auth(smtp.PlainAuth("", n.username, n.password, n.host)); err != nil {
		return fmt.Errorf("auth: %w", err)
	}

	return nil
}

// scrub is the last line of defense: no error built here quotes the password or an address,
// but a server reply may echo an address and a library error might one day quote anything. Only
// when something had to be replaced is the error chain given up, since secrecy outranks
// errors.Is.
func (n *Notifier) scrub(err error) error {
	msg := err.Error()

	clean := msg
	if n.password != "" {
		clean = strings.ReplaceAll(clean, n.password, redacted)
	}

	for _, to := range n.to {
		if to != "" {
			clean = strings.ReplaceAll(clean, to, notify.MaskRecipient(to))
		}
	}

	if clean == msg {
		return err
	}

	return errors.New(clean)
}

// transact sends env as one transaction: MAIL, one RCPT per recipient and, unless every
// recipient was refused, DATA.
func transact(c *smtp.Client, env *envelope, errs []error) error {
	if err := c.Mail(env.from); err != nil {
		return fmt.Errorf("mail from: %w", err)
	}

	accepted, err := recipients(c, env.rcpts, errs)
	if err != nil || accepted == 0 {
		return err
	}

	return send(c, env.data)
}

// recipients issues one RCPT per address and returns how many the server accepted. A refusal
// (an SMTP reply) fails only that address; anything else breaks the transaction and is returned.
func recipients(c *smtp.Client, rcpts []string, errs []error) (int, error) {
	accepted := 0

	for i, rcpt := range rcpts {
		err := c.Rcpt(rcpt)
		if err == nil {
			accepted++

			continue
		}

		if _, refused := errors.AsType[*textproto.Error](err); !refused {
			return accepted, fmt.Errorf("rcpt to: %w", err)
		}

		errs[i] = fmt.Errorf("refused at rcpt to: %w", err)
	}

	return accepted, nil
}

// send transmits the message; net/smtp's writer does the dot-stuffing.
func send(c *smtp.Client, data []byte) error {
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("data: %w", err)
	}

	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("write message: %w", err)
	}

	if err := w.Close(); err != nil {
		return fmt.Errorf("end message: %w", err)
	}

	return nil
}

// fill records err for every recipient that has no outcome yet, keeping a refusal at RCPT as
// the more precise reason.
func fill(errs []error, err error) {
	for i := range errs {
		if errs[i] == nil {
			errs[i] = err
		}
	}
}
