package email

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"github.com/Toshik1978/firefly-jar/internal/digest"
)

// crlf ends every line of the message: SMTP and RFC 5322 both require it.
const crlf = "\r\n"

// errLineBreak rejects a header input that could smuggle extra headers into the message.
var errLineBreak = errors.New("contains CR or LF")

// errInvalidAddress stands in for net/mail's own error, which can quote part of the address.
var errInvalidAddress = errors.New("invalid address")

// envelope is one composed message. rcpts holds the bare address of every valid recipient and
// slots the config position of each; invalid holds, per config position, why that recipient was
// left out (nil when it was kept). data is the whole RFC 5322 message with CRLF line endings, and
// is empty when no recipient is valid.
type envelope struct {
	from    string
	rcpts   []string
	slots   []int
	invalid []error
	data    []byte
}

// compose validates every header input before anything is dialled, then builds the message. An
// invalid From or Subject fails the whole send, since every recipient would get it; an invalid To
// only leaves that recipient out (FR-028). Errors name the field by position, never by value,
// since the value is what is malformed and may be an address.
func compose(from string, to []string, d digest.Digest, now time.Time) (*envelope, error) {
	sender, err := parseAddress("from", from)
	if err != nil {
		return nil, err
	}

	if strings.ContainsAny(d.Subject, "\r\n") {
		return nil, fmt.Errorf("subject: %w", errLineBreak)
	}

	env, valid := parseRecipients(to)
	env.from = sender.Address

	if len(valid) == 0 {
		return env, nil
	}

	if env.data, err = render(sender, valid, d, now); err != nil {
		return nil, err
	}

	return env, nil
}

// parseRecipients sorts the configured To entries into the valid ones, returned in config order
// and recorded in the envelope with their positions, and the invalid ones, recorded with why.
func parseRecipients(to []string) (*envelope, []*mail.Address) {
	env := &envelope{invalid: make([]error, len(to))}
	valid := make([]*mail.Address, 0, len(to))

	for i, raw := range to {
		addr, err := parseAddress("to["+strconv.Itoa(i)+"]", raw)
		if err != nil {
			env.invalid[i] = err

			continue
		}

		valid = append(valid, addr)
		env.rcpts = append(env.rcpts, addr.Address)
		env.slots = append(env.slots, i)
	}

	return env, valid
}

// parseAddress rejects CR and LF before parsing: net/mail would reject most of them too, but the
// guarantee that no header can be injected must not depend on its parser's corner cases.
func parseAddress(field, raw string) (*mail.Address, error) {
	if strings.ContainsAny(raw, "\r\n") {
		return nil, fmt.Errorf("%s: %w", field, errLineBreak)
	}

	addr, err := mail.ParseAddress(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", field, errInvalidAddress)
	}

	return addr, nil
}

// render writes the headers and the quoted-printable body. Quoted-printable keeps the message
// 7-bit and turns every line break of the digest into a hard CRLF.
func render(from *mail.Address, to []*mail.Address, d digest.Digest, now time.Time) ([]byte, error) {
	list := make([]string, 0, len(to))
	for _, addr := range to {
		list = append(list, addr.String())
	}

	var b bytes.Buffer

	writeHeader(&b, "From", from.String())
	writeHeader(&b, "To", strings.Join(list, ", "))
	writeHeader(&b, "Subject", mime.QEncoding.Encode("utf-8", d.Subject))
	writeHeader(&b, "Date", now.Format(time.RFC1123Z))
	writeHeader(&b, "Message-ID", messageID(from.Address))
	writeHeader(&b, "MIME-Version", "1.0")
	writeHeader(&b, "Content-Type", "text/plain; charset=utf-8")
	writeHeader(&b, "Content-Transfer-Encoding", "quoted-printable")
	b.WriteString(crlf)

	qp := quotedprintable.NewWriter(&b)

	if _, err := qp.Write([]byte(d.Text())); err != nil {
		return nil, fmt.Errorf("encode body: %w", err)
	}

	if err := qp.Close(); err != nil {
		return nil, fmt.Errorf("encode body: %w", err)
	}

	return b.Bytes(), nil
}

func writeHeader(b *bytes.Buffer, name, value string) {
	b.WriteString(name + ": " + value + crlf)
}

// messageID is random per message, at the sender's domain, so two digests never share one.
func messageID(sender string) string {
	domain := sender[strings.LastIndexByte(sender, '@')+1:]

	return "<" + rand.Text() + "@" + domain + ">"
}
