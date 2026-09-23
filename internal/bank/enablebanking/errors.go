package enablebanking

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/Toshik1978/firefly-jar/internal/bank"
	"github.com/Toshik1978/firefly-jar/internal/httpx"
)

// Error codes the provider reports in an error body's "error" field that map to a bank sentinel
// regardless of HTTP status (research R7).
const (
	codeRateLimited    = "ASPSP_RATE_LIMIT_EXCEEDED"
	codeExpiredSession = "EXPIRED_SESSION"
	codeRevokedSession = "REVOKED_SESSION"
	codeClosedSession  = "CLOSED_SESSION"
)

// errProvider is the Kind of every provider error that is none of the bank sentinels, so a
// *bank.Error always has a Kind to unwrap to without being mistaken for a consent or rate-limit
// failure.
var errProvider = errors.New("enable banking request failed")

// mapTransportError maps an error from http.Client.Do. An oversized Retry-After is the provider
// throttling us, the same as a final 429. Anything else is wrapped without its *url.Error shell,
// whose message would repeat the full request URL.
func mapTransportError(err error) error {
	if tooLong, ok := errors.AsType[*httpx.RetryAfterTooLongError](err); ok {
		return &bank.Error{
			Kind:   bank.ErrRateLimited,
			Detail: fmt.Sprintf("retry-after %s exceeds the retry cap", tooLong.RetryAfter),
		}
	}

	if urlErr, ok := errors.AsType[*url.Error](err); ok {
		err = urlErr.Err
	}

	return fmt.Errorf("enable banking request: %w", err)
}

// mapErrorResponse maps a non-2xx response to a bank error (research R7). Only the body's "error"
// code and "message" are used, and the message is scrubbed; the raw body never leaves this
// function.
func (c *Client) mapErrorResponse(status int, body []byte) error {
	var parsed errorResponse

	// A body that is not the documented JSON shape still has a status worth reporting, so a
	// decode failure only leaves parsed empty.
	_ = json.Unmarshal(body, &parsed)

	detail := fmt.Sprintf("http %d", status)
	if parsed.Error != "" {
		detail += " " + c.redactor.Scrub(parsed.Error)
	}

	if parsed.Message != "" {
		detail += ": " + c.redactor.Scrub(parsed.Message)
	}

	return &bank.Error{Kind: errorKind(status, parsed.Error), Detail: detail}
}

// errorKind picks the sentinel for an error code, falling back to the HTTP status: a 429 without
// a recognised code is still throttling.
func errorKind(status int, code string) error {
	switch code {
	case codeRateLimited:
		return bank.ErrRateLimited
	case codeExpiredSession:
		return bank.ErrConsentExpired
	case codeRevokedSession, codeClosedSession:
		return bank.ErrConsentRevoked
	}

	if status == http.StatusTooManyRequests {
		return bank.ErrRateLimited
	}

	return errProvider
}
