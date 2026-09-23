// Package httpx builds the outbound HTTP stack shared by every adapter: a retrying
// http.RoundTripper, a per-attempt timeout layer under it, and an *http.Client with no automatic
// redirect handling (R8, R12).
package httpx

import (
	"context"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
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

// RoundTrip implements http.RoundTripper. It retries req against t.Base as documented on
// RetryTransport, and otherwise returns the first attempt's outcome unchanged.
func (t *RetryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := &TimeoutTransport{Base: t.base(), Timeout: t.AttemptTimeout}
	retryable := isRetryableRequest(req)
	ctx := req.Context()

	for attempt := 0; ; attempt++ {
		resp, err := base.RoundTrip(req)
		if err != nil {
			if !retryable || attempt >= t.maxRetries() {
				return nil, fmt.Errorf("httpx: base round trip: %w", err)
			}

			if waitErr := t.wait(ctx, t.backoff(attempt)); waitErr != nil {
				return nil, waitErr
			}

			continue
		}

		retry, actionErr := t.nextResponseAction(ctx, retryable, attempt, resp)
		if actionErr != nil {
			return nil, actionErr
		}

		if !retry {
			return resp, nil
		}
	}
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

// backoff computes the wait before the retry that follows a failed attempt numbered attempt
// (0-indexed): BaseDelay*2^attempt*(0.8+0.4*Jitter()).
func (t *RetryTransport) backoff(attempt int) time.Duration {
	factor := jitterFloor + jitterSpread*t.jitter()

	return time.Duration(float64(t.baseDelay()) * math.Pow(2, float64(attempt)) * factor)
}

// nextResponseAction decides what RoundTrip does with a response that arrived without a network
// error: return it as-is (retry false, err nil), retry it after waiting (retry true), or fail
// immediately because a Retry-After header exceeded MaxWait (retry false, err set). A response
// this discards in favor of a retry is drained and closed first; a response it hands back is
// left untouched for the caller to close.
func (t *RetryTransport) nextResponseAction(
	ctx context.Context, retryable bool, attempt int, resp *http.Response,
) (retry bool, err error) {
	if !retryable || !isRetryableStatus(resp.StatusCode) {
		return false, nil
	}

	delay, tooLongErr := t.delayFor(resp, attempt)
	if tooLongErr != nil {
		drainAndClose(resp.Body)

		return false, fmt.Errorf("httpx: %w", tooLongErr)
	}

	if attempt >= t.maxRetries() {
		return false, nil
	}

	drainAndClose(resp.Body)

	if waitErr := t.wait(ctx, delay); waitErr != nil {
		return false, waitErr
	}

	return true, nil
}

// delayFor returns how long to wait before retrying resp: the computed backoff, or a
// Retry-After header's wait if the response carries one. It reports a *RetryAfterTooLongError
// instead of a delay when that wait exceeds MaxWait.
func (t *RetryTransport) delayFor(resp *http.Response, attempt int) (time.Duration, *RetryAfterTooLongError) {
	header := resp.Header.Get(retryAfterHeader)
	if header == "" {
		return t.backoff(attempt), nil
	}

	retryAfter := parseRetryAfter(header)
	if retryAfter > t.maxWait() {
		return 0, &RetryAfterTooLongError{Status: resp.StatusCode, RetryAfter: retryAfter}
	}

	return retryAfter, nil
}

// wait blocks for delay, or until ctx is done, whichever comes first.
func (t *RetryTransport) wait(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("httpx: wait interrupted: %w", ctx.Err())
	}
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
