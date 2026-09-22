package httpx_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/suite"

	"github.com/Toshik1978/firefly-jar/internal/httpx"
)

// fakeBody is an in-process response body. It records whether the retry transport drained it to
// EOF and closed it, which FR-031 requires for every attempt the transport discards in favor of a
// retry: a failed attempt must never leak a pooled connection.
type fakeBody struct {
	mu     sync.Mutex
	reader *bytes.Reader
	eof    bool
	closed bool
}

func newFakeBody(content string) *fakeBody {
	return &fakeBody{reader: bytes.NewReader([]byte(content))}
}

func (b *fakeBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	n, err := b.reader.Read(p)

	switch {
	case errors.Is(err, io.EOF):
		// Returned as the io package's own sentinel, not the wrapped bytes.Reader error: io.EOF
		// must compare equal for callers such as io.ReadAll, exactly like a real response body.
		b.eof = true

		return n, io.EOF
	case err != nil:
		return n, fmt.Errorf("fake body: read: %w", err)
	default:
		return n, nil
	}
}

func (b *fakeBody) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.closed = true

	return nil
}

func (b *fakeBody) drainedAndClosed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.eof && b.closed
}

func (b *fakeBody) wasClosed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.closed
}

// cappedProbeBodySize is how much content cappedProbeBody can yield if read to EOF: far more
// than any reasonable drain cap, so a test can prove drainAndClose stops well short of it.
const cappedProbeBodySize = 10 * (64 << 10)

// cappedProbeBody is an in-process io.ReadCloser that would, read to EOF, yield
// cappedProbeBodySize bytes. It records how many bytes were actually read and whether it was
// closed, so a test can assert that a discarded attempt's body is drained only up to a bounded
// cap, never in full, before being closed.
type cappedProbeBody struct {
	mu     sync.Mutex
	read   int
	closed bool
}

func (b *cappedProbeBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	remaining := cappedProbeBodySize - b.read
	if remaining <= 0 {
		return 0, io.EOF
	}

	n := min(len(p), remaining)

	for i := range n {
		p[i] = 'x'
	}

	b.read += n

	return n, nil
}

func (b *cappedProbeBody) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.closed = true

	return nil
}

func (b *cappedProbeBody) bytesRead() int {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.read
}

func (b *cappedProbeBody) wasClosed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.closed
}

// scriptedStep is one queued outcome of a fakeTransport: either a response or a network error,
// never both. A response's body is normally built from body, but rawBody lets a test supply its
// own io.ReadCloser instead, for cases (such as a body far larger than a cap) that fakeBody's
// fixed in-memory buffer cannot represent.
type scriptedStep struct {
	err     error
	header  http.Header
	body    string
	rawBody io.ReadCloser
	status  int
}

// fakeAttempt records one call the code under test made to fakeTransport.RoundTrip: which HTTP
// method it used, and the bubble's fake time it happened at.
type fakeAttempt struct {
	at     time.Time
	method string
}

// fakeTransport is an in-process, synctest-safe stand-in for the network (per .claude/CLAUDE.md:
// no httptest.Server and no sockets inside a synctest bubble). It returns one scripted step per
// call, in order, and records every attempt so a test can assert both what was sent and when.
type fakeTransport struct {
	mu       sync.Mutex
	steps    []scriptedStep
	attempts []fakeAttempt
	bodies   []*fakeBody
}

func (f *fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	idx := len(f.attempts)
	f.attempts = append(f.attempts, fakeAttempt{method: req.Method, at: time.Now()})

	if idx >= len(f.steps) {
		f.mu.Unlock()

		return nil, fmt.Errorf("fakeTransport: no scripted step for attempt %d", idx)
	}

	step := f.steps[idx]
	f.mu.Unlock()

	if step.err != nil {
		return nil, step.err
	}

	var body io.ReadCloser
	if step.rawBody != nil {
		body = step.rawBody
	} else {
		fb := newFakeBody(step.body)

		f.mu.Lock()
		f.bodies = append(f.bodies, fb)
		f.mu.Unlock()

		body = fb
	}

	header := step.header.Clone()
	if header == nil {
		header = make(http.Header)
	}

	return &http.Response{
		Status:     strconv.Itoa(step.status),
		StatusCode: step.status,
		Header:     header,
		Body:       body,
		Request:    req,
	}, nil
}

func (f *fakeTransport) attemptSnapshot() []fakeAttempt {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]fakeAttempt(nil), f.attempts...)
}

func (f *fakeTransport) bodyAt(i int) *fakeBody {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.bodies[i]
}

// fixedJitter builds a Jitter func that always returns v, so RetryTransport's backoff is exactly
// BaseDelay*2^n*(0.8+0.4*v) rather than a range. The brief pins v=0.5 to the schedule 1s, 2s, 4s.
func fixedJitter(v float64) func() float64 {
	return func() float64 { return v }
}

func newGetRequest(ctx context.Context, method string) *http.Request {
	req, err := http.NewRequestWithContext(ctx, method, "https://example.com/api/v1/accounts", http.NoBody)
	if err != nil {
		// Building a request from a constant, valid URL never fails; a failure here is a bug in
		// the test helper, not in the code under test.
		panic(err)
	}

	return req
}

// RetrySuite covers httpx.RetryTransport (T019, R12, FR-031): which failures are retried, the
// backoff and Retry-After schedule, the retry cap, and the resource and cancellation guarantees
// around a retried attempt.
//
// Every case runs its RoundTrip call inside a testing/synctest bubble against fakeTransport, so
// the 1s/2s/4s backoff and any Retry-After wait are exact and instant rather than real sleeps.
// synctest.Test takes the bubble's own *testing.T, which cannot be shared with s (a different
// *testing.T, running outside the bubble): every case therefore only *collects* observations
// (attempt records, status codes, body text, errors, elapsed time -- plain values, never the
// *http.Response itself) into variables declared outside the closure, and asserts on them with
// suite methods after synctest.Test has returned. Each response body is also read and closed
// inside the same closure that made the call: closing it afterwards, from the enclosing method,
// would be invisible to the bodyclose linter, whose analysis does not follow a *http.Response
// captured by a closure and released to its enclosing function. A bare synchronous helper failure
// inside the bubble (e.g. building the request) uses the bubble's own t, since that is a
// test-setup bug rather than a behavior under test.
type RetrySuite struct {
	suite.Suite
}

func (s *RetrySuite) TestSucceedsAfterTwoRetriesWithOneAndTwoSecondBackoff() {
	var (
		attempts                            []fakeAttempt
		start                               time.Time
		statusCode                          int
		bodyText                            string
		respErr                             error
		firstBodyDrained, secondBodyDrained bool
		finalBodyClosedByTransport          bool
	)

	synctest.Test(s.T(), func(t *testing.T) {
		fake := &fakeTransport{steps: []scriptedStep{
			{status: http.StatusInternalServerError, body: "boom-1"},
			{status: http.StatusInternalServerError, body: "boom-2"},
			{status: http.StatusOK, body: "ok"},
		}}
		rt := &httpx.RetryTransport{Base: fake, Jitter: fixedJitter(0.5)}

		start = time.Now()

		resp, err := rt.RoundTrip(newGetRequest(t.Context(), http.MethodGet))
		respErr = err
		attempts = fake.attemptSnapshot()
		firstBodyDrained = fake.bodyAt(0).drainedAndClosed()
		secondBodyDrained = fake.bodyAt(1).drainedAndClosed()

		if resp == nil {
			return
		}

		// Captured before this test's own read and close below, so it reflects only what
		// RetryTransport itself did with the final, returned attempt's body.
		finalBodyClosedByTransport = fake.bodyAt(2).wasClosed()

		defer func() { _ = resp.Body.Close() }()

		statusCode = resp.StatusCode

		data, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			t.Fatal(readErr)
		}

		bodyText = string(data)
	})

	s.Require().NoError(respErr)
	s.Equal(http.StatusOK, statusCode)

	s.Require().Len(attempts, 3, "two retries means three attempts total")
	s.Equal(start, attempts[0].at, "the first attempt is immediate")
	s.Equal(start.Add(1*time.Second), attempts[1].at, "jitter 0.5 makes the first backoff exactly 1s")
	s.Equal(start.Add(3*time.Second), attempts[2].at, "second backoff is exactly 2s, so 1s+2s after start")

	s.True(firstBodyDrained, "the first 500's body must be drained and closed before retrying")
	s.True(secondBodyDrained, "the second 500's body must be drained and closed before retrying")
	s.False(finalBodyClosedByTransport, "the final, returned response body must be left for the caller to close")
	s.Equal("ok", bodyText)
}

// drainCapBytes mirrors the cap httpx.RetryTransport documents for a discarded body's drain: a
// body far larger than this must never be read in full before being closed.
const drainCapBytes = 64 << 10

func (s *RetrySuite) TestDiscardedBodyDrainIsCappedThenClosed() {
	var (
		attempts       []fakeAttempt
		probeBytesRead int
		probeClosed    bool
	)

	synctest.Test(s.T(), func(t *testing.T) {
		probe := &cappedProbeBody{}
		fake := &fakeTransport{steps: []scriptedStep{
			{status: http.StatusInternalServerError, rawBody: probe},
			{status: http.StatusOK, body: "ok"},
		}}
		rt := &httpx.RetryTransport{Base: fake, Jitter: fixedJitter(0.5)}

		resp, err := rt.RoundTrip(newGetRequest(t.Context(), http.MethodGet))
		if err != nil {
			t.Fatal(err)
		}

		if resp != nil {
			_ = resp.Body.Close()
		}

		attempts = fake.attemptSnapshot()
		probeBytesRead = probe.bytesRead()
		probeClosed = probe.wasClosed()
	})

	s.Require().Len(attempts, 2, "a capped drain must not block the retry")
	s.LessOrEqual(
		probeBytesRead, drainCapBytes,
		"a discarded body must never be read past the drain cap, however much content it holds",
	)
	s.True(probeClosed, "a discarded body must still be closed once its capped drain finishes")
}

func (s *RetrySuite) TestNetworkErrorIsRetried() {
	var (
		attempts   []fakeAttempt
		start      time.Time
		statusCode int
		respErr    error
	)

	synctest.Test(s.T(), func(t *testing.T) {
		fake := &fakeTransport{steps: []scriptedStep{
			{err: errors.New("dial tcp 203.0.113.1:443: connect: connection refused")},
			{status: http.StatusOK, body: "ok"},
		}}
		rt := &httpx.RetryTransport{Base: fake, Jitter: fixedJitter(0.5)}

		start = time.Now()

		resp, err := rt.RoundTrip(newGetRequest(t.Context(), http.MethodGet))
		respErr = err
		attempts = fake.attemptSnapshot()

		if resp != nil {
			statusCode = resp.StatusCode

			_ = resp.Body.Close()
		}
	})

	s.Require().NoError(respErr)
	s.Equal(http.StatusOK, statusCode)
	s.Require().Len(attempts, 2)
	s.Equal(start.Add(1*time.Second), attempts[1].at, "a network error backs off exactly like a 5xx")
}

func (s *RetrySuite) TestHEADIsRetried() {
	var (
		attempts   []fakeAttempt
		start      time.Time
		statusCode int
		respErr    error
	)

	synctest.Test(s.T(), func(t *testing.T) {
		fake := &fakeTransport{steps: []scriptedStep{
			{status: http.StatusInternalServerError, body: ""},
			{status: http.StatusOK, body: ""},
		}}
		rt := &httpx.RetryTransport{Base: fake, Jitter: fixedJitter(0.5)}

		start = time.Now()

		resp, err := rt.RoundTrip(newGetRequest(t.Context(), http.MethodHead))
		respErr = err
		attempts = fake.attemptSnapshot()

		if resp != nil {
			statusCode = resp.StatusCode

			_ = resp.Body.Close()
		}
	})

	s.Require().NoError(respErr)
	s.Equal(http.StatusOK, statusCode)
	s.Require().Len(attempts, 2, "HEAD is idempotent and must be retried like GET")
	s.Equal(start.Add(1*time.Second), attempts[1].at, "a HEAD retry backs off exactly like a GET retry")
}

func (s *RetrySuite) TestRetryAfterSecondsHeaderReplacesTheBackoff() {
	var (
		attempts []fakeAttempt
		start    time.Time
		respErr  error
	)

	synctest.Test(s.T(), func(t *testing.T) {
		header := make(http.Header)
		header.Set("Retry-After", "2")
		fake := &fakeTransport{steps: []scriptedStep{
			{status: http.StatusTooManyRequests, header: header, body: "rate limited"},
			{status: http.StatusOK, body: "ok"},
		}}
		// Jitter 0.99 would push the computed backoff toward 1.4s if it were used; pinning the
		// wait to exactly 2s proves the header overrides the computed backoff rather than adding
		// to it.
		rt := &httpx.RetryTransport{Base: fake, Jitter: fixedJitter(0.99)}

		start = time.Now()

		resp, err := rt.RoundTrip(newGetRequest(t.Context(), http.MethodGet))
		respErr = err
		attempts = fake.attemptSnapshot()

		if resp != nil {
			_ = resp.Body.Close()
		}
	})

	s.Require().NoError(respErr)
	s.Require().Len(attempts, 2)
	s.Equal(start.Add(2*time.Second), attempts[1].at, "Retry-After: 2 must wait exactly 2s, not a jittered backoff")
}

func (s *RetrySuite) TestRetryAfterHTTPDateIsHonored() {
	var (
		attempts []fakeAttempt
		start    time.Time
		retryAt  time.Time
		respErr  error
	)

	synctest.Test(s.T(), func(t *testing.T) {
		start = time.Now()
		retryAt = start.Add(5 * time.Second)

		header := make(http.Header)
		header.Set("Retry-After", retryAt.UTC().Format(http.TimeFormat))
		fake := &fakeTransport{steps: []scriptedStep{
			{status: http.StatusTooManyRequests, header: header, body: "rate limited"},
			{status: http.StatusOK, body: "ok"},
		}}
		rt := &httpx.RetryTransport{Base: fake, Jitter: fixedJitter(0.5)}

		resp, err := rt.RoundTrip(newGetRequest(t.Context(), http.MethodGet))
		respErr = err
		attempts = fake.attemptSnapshot()

		if resp != nil {
			_ = resp.Body.Close()
		}
	})

	s.Require().NoError(respErr)
	s.Require().Len(attempts, 2)
	s.Equal(retryAt, attempts[1].at, "an HTTP-date Retry-After must be honored exactly")
}

func (s *RetrySuite) TestRetryAfterNegativeSecondsIsClampedToAnImmediateRetry() {
	var (
		attempts []fakeAttempt
		start    time.Time
		respErr  error
	)

	synctest.Test(s.T(), func(t *testing.T) {
		header := make(http.Header)
		header.Set("Retry-After", "-5")
		fake := &fakeTransport{steps: []scriptedStep{
			{status: http.StatusTooManyRequests, header: header, body: "rate limited"},
			{status: http.StatusOK, body: "ok"},
		}}
		rt := &httpx.RetryTransport{Base: fake, Jitter: fixedJitter(0.5)}

		start = time.Now()

		resp, err := rt.RoundTrip(newGetRequest(t.Context(), http.MethodGet))
		respErr = err
		attempts = fake.attemptSnapshot()

		if resp != nil {
			_ = resp.Body.Close()
		}
	})

	s.Require().NoError(respErr)
	s.Require().Len(attempts, 2)
	s.Equal(
		start,
		attempts[1].at,
		"a negative Retry-After must be clamped to an immediate retry, never a negative wait",
	)
}

func (s *RetrySuite) TestRetryAfterOverTheCapStopsAtOnceWithRetryAfterTooLongError() {
	var (
		attempts   []fakeAttempt
		elapsed    time.Duration
		respWasNil bool
		respErr    error
	)

	synctest.Test(s.T(), func(t *testing.T) {
		header := make(http.Header)
		header.Set("Retry-After", "120")
		fake := &fakeTransport{steps: []scriptedStep{
			{status: http.StatusTooManyRequests, header: header, body: "rate limited"},
		}}
		rt := &httpx.RetryTransport{Base: fake, Jitter: fixedJitter(0.5), MaxWait: 60 * time.Second}

		start := time.Now()

		resp, err := rt.RoundTrip(newGetRequest(t.Context(), http.MethodGet))
		respErr = err
		elapsed = time.Since(start)
		attempts = fake.attemptSnapshot()
		respWasNil = resp == nil

		if resp != nil {
			_ = resp.Body.Close()
		}
	})

	s.Require().Len(attempts, 1, "a Retry-After over the cap must stop before a second attempt")
	s.Equal(time.Duration(0), elapsed, "a wait longer than the cap must never be waited out, even partially")
	s.True(respWasNil, "a Retry-After over the cap must return no response, only the error")

	s.Require().Error(respErr)

	var tooLong *httpx.RetryAfterTooLongError
	s.Require().ErrorAs(respErr, &tooLong)
	s.Equal(http.StatusTooManyRequests, tooLong.Status)
	s.Equal(120*time.Second, tooLong.RetryAfter)
	s.Contains(tooLong.Error(), strconv.Itoa(http.StatusTooManyRequests))
	s.Contains(tooLong.Error(), (120 * time.Second).String())
}

func (s *RetrySuite) TestClientErrorStatusesAreNeverRetried() {
	statuses := []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound}

	for _, status := range statuses {
		s.Run(strconv.Itoa(status), func() {
			var (
				attempts   []fakeAttempt
				statusCode int
				respErr    error
			)

			synctest.Test(s.T(), func(t *testing.T) {
				fake := &fakeTransport{steps: []scriptedStep{
					{status: status, body: "client error"},
					{status: http.StatusOK, body: "unreachable if retried"},
				}}
				rt := &httpx.RetryTransport{Base: fake, Jitter: fixedJitter(0.5)}

				resp, err := rt.RoundTrip(newGetRequest(t.Context(), http.MethodGet))
				respErr = err
				attempts = fake.attemptSnapshot()

				if resp != nil {
					statusCode = resp.StatusCode

					_ = resp.Body.Close()
				}
			})

			s.Require().NoError(respErr)
			s.Equal(status, statusCode)
			s.Require().Len(attempts, 1, "a non-429 4xx must never be retried")
		})
	}
}

func (s *RetrySuite) TestPOSTIsNeverRetried() {
	var (
		attempts   []fakeAttempt
		statusCode int
		respErr    error
	)

	synctest.Test(s.T(), func(t *testing.T) {
		fake := &fakeTransport{steps: []scriptedStep{
			{status: http.StatusInternalServerError, body: "boom"},
			{status: http.StatusOK, body: "unreachable if retried"},
		}}
		rt := &httpx.RetryTransport{Base: fake, Jitter: fixedJitter(0.5)}

		resp, err := rt.RoundTrip(newGetRequest(t.Context(), http.MethodPost))
		respErr = err
		attempts = fake.attemptSnapshot()

		if resp != nil {
			statusCode = resp.StatusCode

			_ = resp.Body.Close()
		}
	})

	s.Require().NoError(respErr)
	s.Equal(http.StatusInternalServerError, statusCode, "a non-idempotent POST is never retried, even on a 5xx")
	s.Require().Len(attempts, 1)
}

func (s *RetrySuite) TestContextCancellationStopsTheWaitAndReturnsCtxErr() {
	var (
		respErr error
		elapsed time.Duration
	)

	synctest.Test(s.T(), func(t *testing.T) {
		fake := &fakeTransport{steps: []scriptedStep{
			{status: http.StatusInternalServerError, body: "boom-1"},
			{status: http.StatusInternalServerError, body: "boom-2"},
			{status: http.StatusOK, body: "ok"},
		}}
		rt := &httpx.RetryTransport{Base: fake, Jitter: fixedJitter(0.5)}

		ctx, cancel := context.WithCancel(t.Context())
		req := newGetRequest(ctx, http.MethodGet)

		start := time.Now()
		done := make(chan struct{})

		go func() {
			defer close(done)

			resp, err := rt.RoundTrip(req)
			respErr = err

			if resp != nil {
				_ = resp.Body.Close()
			}
		}()

		// Let the first (failing) attempt happen and the retry loop settle into its 1s backoff
		// wait, which is the only way it can become durably blocked.
		synctest.Wait()
		cancel()
		<-done

		elapsed = time.Since(start)
	})

	s.Require().Error(respErr)
	s.Require().ErrorIs(respErr, context.Canceled)
	s.Equal(time.Duration(0), elapsed, "cancellation must interrupt the wait rather than let any of it finish")
}

func (s *RetrySuite) TestGivesUpAfterMaxRetriesReturningTheLastResponse() {
	var (
		attempts   []fakeAttempt
		statusCode int
		bodyText   string
		respErr    error
	)

	synctest.Test(s.T(), func(t *testing.T) {
		fake := &fakeTransport{steps: []scriptedStep{
			{status: http.StatusInternalServerError, body: "boom-1"},
			{status: http.StatusInternalServerError, body: "boom-2"},
			{status: http.StatusInternalServerError, body: "boom-3"},
			{status: http.StatusInternalServerError, body: "boom-4"},
			{status: http.StatusInternalServerError, body: "unreachable, a 5th attempt is one retry too many"},
		}}
		rt := &httpx.RetryTransport{Base: fake, Jitter: fixedJitter(0.5)}

		resp, err := rt.RoundTrip(newGetRequest(t.Context(), http.MethodGet))
		respErr = err
		attempts = fake.attemptSnapshot()

		if resp == nil {
			return
		}

		defer func() { _ = resp.Body.Close() }()

		statusCode = resp.StatusCode

		data, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			t.Fatal(readErr)
		}

		bodyText = string(data)
	})

	s.Require().NoError(respErr)
	s.Require().Len(attempts, 4, "3 retries means 4 attempts total, then giving up")
	s.Equal(http.StatusInternalServerError, statusCode)
	s.Equal("boom-4", bodyText, "the last attempt's response must be the one returned, and still readable")
}

func (s *RetrySuite) TestGivesUpAfterMaxRetriesReturningTheLastError() {
	lastErr := errors.New("boom-4: connection reset")

	var (
		attempts []fakeAttempt
		respErr  error
	)

	synctest.Test(s.T(), func(t *testing.T) {
		fake := &fakeTransport{steps: []scriptedStep{
			{err: errors.New("boom-1: connection reset")},
			{err: errors.New("boom-2: connection reset")},
			{err: errors.New("boom-3: connection reset")},
			{err: lastErr},
		}}
		rt := &httpx.RetryTransport{Base: fake, Jitter: fixedJitter(0.5)}

		resp, err := rt.RoundTrip(newGetRequest(t.Context(), http.MethodGet))
		respErr = err
		attempts = fake.attemptSnapshot()

		if resp != nil {
			_ = resp.Body.Close()
		}
	})

	s.Require().Error(respErr)
	s.Require().Len(attempts, 4)
	s.Require().ErrorIs(respErr, lastErr, "the last error must still be reachable via errors.Is after wrapping")
}

// TestZeroValueUsesTheDocumentedDefaults pins RetryTransport's documented zero-value behavior
// (MaxRetries 3, BaseDelay 1s, MaxWait 60s, Jitter = a 20% source): with no Jitter injected, the
// exact wait is not deterministic, so this asserts each wait falls inside the documented +/-20%
// band around the 1s/2s default schedule instead of pinning it to one number.
func (s *RetrySuite) TestZeroValueUsesTheDocumentedDefaults() {
	var attempts []fakeAttempt

	synctest.Test(s.T(), func(t *testing.T) {
		fake := &fakeTransport{steps: []scriptedStep{
			{status: http.StatusInternalServerError, body: "boom-1"},
			{status: http.StatusInternalServerError, body: "boom-2"},
			{status: http.StatusOK, body: "ok"},
		}}
		rt := &httpx.RetryTransport{Base: fake}

		resp, err := rt.RoundTrip(newGetRequest(t.Context(), http.MethodGet))
		if err != nil {
			t.Fatal(err)
		}

		if resp != nil {
			_ = resp.Body.Close()
		}

		attempts = fake.attemptSnapshot()
	})

	s.Require().Len(attempts, 3)

	firstWait := attempts[1].at.Sub(attempts[0].at)
	secondWait := attempts[2].at.Sub(attempts[1].at)
	s.InDelta(1*time.Second, firstWait, float64(200*time.Millisecond), "default BaseDelay is 1s +/- 20%% jitter")
	s.InDelta(2*time.Second, secondWait, float64(400*time.Millisecond), "default backoff doubles to 2s +/- 20%% jitter")
}
