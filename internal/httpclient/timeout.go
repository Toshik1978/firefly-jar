package httpclient

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// defaultAttemptTimeout bounds one HTTP attempt, from sending the request until its response body
// is closed (R12: "per-request timeout is 30 s"). It is per attempt, not per exchange: a retry, and
// the backoff or Retry-After wait before it, starts a fresh bound, and only the run's own context
// (the 10-minute run cap) bounds the exchange as a whole.
const defaultAttemptTimeout = 30 * time.Second

// TimeoutTransport wraps Base and gives every request it forwards its own deadline, derived from
// the request's context. The deadline stays armed after the headers arrive and is released only
// when the response body is closed, so it also caps how long the body read can take: a server that
// sends headers and then stalls is cut off like one that never answers. It never retries.
type TimeoutTransport struct {
	// Base is the underlying transport. A nil Base uses http.DefaultTransport.
	Base http.RoundTripper
	// Timeout bounds one request, body read included. Zero uses the default of 30s.
	Timeout time.Duration
}

// RoundTrip implements http.RoundTripper. It sends req to Base under a deadline of t's Timeout
// and hands the response back with a body whose Close releases that deadline.
func (t *TimeoutTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}

	timeout := t.Timeout
	if timeout == 0 {
		timeout = defaultAttemptTimeout
	}

	ctx, cancel := context.WithTimeout(req.Context(), timeout)

	resp, err := base.RoundTrip(req.WithContext(ctx))
	if err != nil {
		cancel()

		return nil, fmt.Errorf("httpclient: round trip: %w", err)
	}

	resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}

	return resp, nil
}

// cancelOnClose releases an attempt's deadline when its response body is closed, the point at
// which the caller is done with the attempt.
type cancelOnClose struct {
	io.ReadCloser

	cancel context.CancelFunc
}

// Close closes the underlying body, then releases the attempt's deadline.
func (b *cancelOnClose) Close() error {
	defer b.cancel()

	if err := b.ReadCloser.Close(); err != nil {
		return fmt.Errorf("httpclient: close body: %w", err)
	}

	return nil
}
