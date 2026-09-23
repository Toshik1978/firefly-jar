// Package httpclient builds the outbound HTTP stack shared by every adapter: a retrying
// http.RoundTripper, a per-attempt timeout layer under it, and an *http.Client with no automatic
// redirect handling (R8, R12).
package httpclient

import (
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"

	"github.com/avast/retry-go/v5"
)

// Defaults applied whenever the matching RetryTransport field is left at its zero value: 3
// retries, a 1s base delay, a 60s Retry-After cap, and a jitter source of 20% (R12).
const (
	defaultMaxRetries = 3
	defaultBaseDelay  = time.Second
	defaultMaxWait    = 60 * time.Second
	jitterFloor       = 0.8
	jitterSpread      = 0.4
)

// retryAfterHeader is the response header a server uses to replace the computed backoff with an
// exact wait, either delta-seconds or an HTTP-date (RFC 9110 §10.2.3).
const retryAfterHeader = "Retry-After"

// RetryTransport wraps Base with bounded retries for GET and HEAD requests: network errors, 5xx
// and 429 responses are retried with exponential backoff, up to MaxRetries times. A Retry-After
// response header replaces the computed backoff, capped at MaxWait. Every attempt, retried or not,
// runs under its own AttemptTimeout (see TimeoutTransport), so a hung attempt is cut off and
// retried while the waits between attempts are bounded only by the request's context. Every field
// is optional; the zero value uses the documented defaults (R12).
//
// The attempt loop and the wait between attempts are github.com/avast/retry-go's. Everything that
// makes it an HTTP retry is here: which requests and outcomes are retried, the delay, the
// Retry-After cap, draining discarded bodies, and handing the last response back.
type RetryTransport struct {
	// Base is the underlying transport. A nil Base uses http.DefaultTransport.
	Base http.RoundTripper
	// Jitter returns a value in [0, 1) used to randomize each backoff. A nil Jitter uses
	// math/rand/v2's Float64.
	Jitter func() float64
	// MaxRetries caps how many times a retryable request is retried, on top of its first
	// attempt. Zero uses the default of 3.
	MaxRetries int
	// BaseDelay is the backoff before the first retry, doubled for every retry after that.
	// Zero uses the default of 1s.
	BaseDelay time.Duration
	// MaxWait caps how long a Retry-After header is honored. A longer request fails immediately
	// instead of waiting it out. Zero uses the default of 60s.
	MaxWait time.Duration
	// AttemptTimeout bounds one attempt, its response body read included. Zero uses the default
	// of 30s.
	AttemptTimeout time.Duration
}

// RetryAfterTooLongError reports that a server's Retry-After header exceeded RetryTransport's
// MaxWait, so the request was given up on immediately rather than waiting.
type RetryAfterTooLongError struct {
	// RetryAfter is the wait the server requested.
	RetryAfter time.Duration
	// Status is the HTTP status code of the response that carried the header.
	Status int
}

// Error implements the error interface for RetryAfterTooLongError.
func (e *RetryAfterTooLongError) Error() string {
	return fmt.Sprintf("retry-after %s for status %d exceeds the retry cap", e.RetryAfter, e.Status)
}

// attemptError carries one failed attempt through retry-go, whose loop sees only errors. err is
// already worded for RoundTrip's caller. When hasRetryAfter is set, retryAfter is the wait the
// server asked for, and it replaces the backoff before the next attempt.
type attemptError struct {
	err           error
	retryAfter    time.Duration
	hasRetryAfter bool
}

// Error implements the error interface for attemptError.
func (e *attemptError) Error() string {
	return e.err.Error()
}

// Unwrap exposes the caller-facing error, so errors.Is and errors.As see through an attemptError.
func (e *attemptError) Unwrap() error {
	return e.err
}

// RoundTrip implements http.RoundTripper. It retries req against t.Base as documented on
// RetryTransport, and otherwise returns the first attempt's outcome unchanged.
func (t *RetryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := &TimeoutTransport{Base: t.base(), Timeout: t.AttemptTimeout}

	if !isRetryableRequest(req) {
		resp, err := base.RoundTrip(req)
		if err != nil {
			return nil, fmt.Errorf("httpclient: base round trip: %w", err)
		}

		return resp, nil
	}

	// retry-go returns no value alongside its final error, so the loop counts attempts itself:
	// the final attempt hands a retryable response back as a success instead of failing with it.
	attempts := t.attempts()
	made := uint(0)

	resp, err := retry.NewWithData[*http.Response](
		retry.Attempts(attempts),
		retry.Context(req.Context()),
		retry.Delay(t.baseDelay()),
		retry.DelayType(t.delay),
		// Without it retry-go gives up with every attempt's error joined into one retry.Error,
		// whose errors.As finds the first attempt's failure rather than the last.
		retry.LastErrorOnly(true),
	).Do(func() (*http.Response, error) {
		made++

		return t.attempt(base, req, made == attempts)
	})
	if err != nil {
		return nil, finalError(err)
	}

	return resp, nil
}

// base returns t.Base, or http.DefaultTransport if it is nil.
func (t *RetryTransport) base() http.RoundTripper {
	if t.Base != nil {
		return t.Base
	}

	return http.DefaultTransport
}

// maxRetries returns t.MaxRetries, or the documented default if it is zero.
func (t *RetryTransport) maxRetries() int {
	if t.MaxRetries != 0 {
		return t.MaxRetries
	}

	return defaultMaxRetries
}

// attempts returns how many attempts one request gets: the first, plus maxRetries retries. A
// negative MaxRetries means no retry at all, never retry-go's Attempts(0), which retries forever.
func (t *RetryTransport) attempts() uint {
	return uint(max(t.maxRetries(), 0)) + 1
}

// baseDelay returns t.BaseDelay, or the documented default if it is zero.
func (t *RetryTransport) baseDelay() time.Duration {
	if t.BaseDelay != 0 {
		return t.BaseDelay
	}

	return defaultBaseDelay
}

// maxWait returns t.MaxWait, or the documented default if it is zero.
func (t *RetryTransport) maxWait() time.Duration {
	if t.MaxWait != 0 {
		return t.MaxWait
	}

	return defaultMaxWait
}

// jitter returns one value from t.Jitter, or from math/rand/v2's Float64 if it is nil.
func (t *RetryTransport) jitter() float64 {
	if t.Jitter != nil {
		return t.Jitter()
	}

	return rand.Float64() //nolint:gosec // jitter for a retry backoff is not a security context
}

// delay is retry-go's DelayType. Before retry n (1-based) after a failed attempt err, it waits
// what the server asked for in a Retry-After header or, without one, the exponential backoff
// BaseDelay*2^(n-1)*(0.8+0.4*Jitter()). retry-go's own jitter is only ever additive, never ±20%.
func (t *RetryTransport) delay(n uint, err error, config retry.DelayContext) time.Duration {
	if failed, ok := errors.AsType[*attemptError](err); ok && failed.hasRetryAfter {
		return failed.retryAfter
	}

	factor := jitterFloor + jitterSpread*t.jitter()

	return time.Duration(float64(config.Delay()) * math.Pow(2, float64(n-1)) * factor)
}

// attempt makes one attempt and tells retry-go what became of it. It returns the response when
// there is nothing to retry: a non-retryable status, or a retryable one on the final attempt. It
// fails with an *attemptError to have retry-go wait and try again, or with an unrecoverable one
// when a Retry-After header exceeds MaxWait. A response it discards is drained and closed first;
// a response it returns is left untouched for the caller to close.
func (t *RetryTransport) attempt(base http.RoundTripper, req *http.Request, final bool) (*http.Response, error) {
	resp, err := base.RoundTrip(req)
	if err != nil {
		return nil, &attemptError{err: fmt.Errorf("httpclient: base round trip: %w", err)}
	}

	if !isRetryableStatus(resp.StatusCode) {
		return resp, nil
	}

	failed := &attemptError{err: fmt.Errorf("httpclient: retryable status %d", resp.StatusCode)}

	if header := resp.Header.Get(retryAfterHeader); header != "" {
		failed.retryAfter, failed.hasRetryAfter = parseRetryAfter(header), true

		if failed.retryAfter > t.maxWait() {
			drainAndClose(resp.Body)

			tooLong := &RetryAfterTooLongError{Status: resp.StatusCode, RetryAfter: failed.retryAfter}

			// retry-go looks for Unrecoverable anywhere in the chain, so marking the caller-facing
			// error stops the loop at once and still leaves the *RetryAfterTooLongError reachable.
			return nil, &attemptError{err: retry.Unrecoverable(fmt.Errorf("httpclient: %w", tooLong))}
		}
	}

	if final {
		return resp, nil
	}

	drainAndClose(resp.Body)

	return nil, failed
}

// finalError turns the error retry-go gives up with into the one RoundTrip reports. An attempt
// fails only with an *attemptError, already worded for the caller. Any other error is the
// request's context, which retry-go returns when it ends during a wait, or before the first
// attempt when it had already ended.
func finalError(err error) error {
	if failed, ok := errors.AsType[*attemptError](err); ok {
		return failed.err
	}

	return fmt.Errorf("httpclient: request context ended: %w", err)
}

// isRetryableRequest reports whether req is eligible for a retry at all: only GET and HEAD are
// idempotent enough to replay, and only a bodyless request can be replayed without a GetBody.
func isRetryableRequest(req *http.Request) bool {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return false
	}

	return req.Body == nil || req.Body == http.NoBody
}

// isRetryableStatus reports whether status is retried: 429, or any 5xx.
func isRetryableStatus(status int) bool {
	return status == http.StatusTooManyRequests || (status >= 500 && status <= 599)
}

// parseRetryAfter parses a Retry-After header value as delta-seconds or an HTTP-date, returning
// zero for a value in neither form.
func parseRetryAfter(header string) time.Duration {
	if seconds, err := strconv.Atoi(header); err == nil {
		if seconds < 0 {
			// A negative delta-seconds value is malformed per RFC 9110, but a malformed header
			// must never turn into a negative wait: clamp it to "retry immediately" instead.
			return 0
		}

		return time.Duration(seconds) * time.Second
	}

	if when, err := http.ParseTime(header); err == nil {
		if until := time.Until(when); until > 0 {
			return until
		}
	}

	return 0
}

// drainCapBytes bounds how much of a discarded attempt's body drainAndClose reads before
// closing it. Draining fully is only needed to let the underlying transport reuse the
// connection; an unbounded read of a large or slow body would block a retry indefinitely for a
// connection that is just as reusable after a bounded drain.
const drainCapBytes = 64 << 10

// drainAndClose reads up to drainCapBytes of body, then closes it, so a discarded attempt's
// connection can be reused by the transport's pool instead of being torn down. The copy and the
// close can each fail for reasons outside this transport's control (a wedged connection, a body
// already closed elsewhere); the body is being discarded either way, so neither error changes
// what RoundTrip reports to its caller, and both are deliberately ignored.
func drainAndClose(body io.ReadCloser) {
	_, _ = io.CopyN(io.Discard, body, drainCapBytes)
	_ = body.Close()
}
