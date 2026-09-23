package httpclient_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"slices"
	"strconv"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jarcoal/httpmock"
	"github.com/stretchr/testify/suite"

	"github.com/Toshik1978/firefly-jar/internal/httpclient"
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

// stalledBody is a response body whose every Read blocks until done is closed and then fails with
// cause's error: given a request context's Done and Err, it fails the way a real transport's body
// read does once its request context ends.
type stalledBody struct {
	done  <-chan struct{}
	cause func() error
}

func (b *stalledBody) Read(_ []byte) (int, error) {
	<-b.done

	return 0, fmt.Errorf("stalled body: %w", b.cause())
}

func (b *stalledBody) Close() error { return nil }

// testURL is the only URL a scripted transport answers, and the one newGetRequest targets.
const testURL = "https://example.com/api/v1/accounts"

// hangDelay is how long a hung step holds its answer back: far longer than the 30 s attempt
// timeout and every wait these cases make, so the attempt's context always ends first. httpmock's
// Delay then fails the attempt with the context's error, the in-process shape of a server that
// accepted the connection and never answered.
const hangDelay = 24 * time.Hour

// hangs is a step that never answers before its attempt's context ends.
func hangs() httpmock.Responder {
	return httpmock.NewStringResponder(http.StatusOK, "too late").Delay(hangDelay)
}

// withBody is a step answering status with body, an instrumented body a test inspects afterwards.
// httpmock's own bodies record neither how far they were read nor whether they were closed, and
// its Close seeks to the end, so a body closed unread would pass for a drained one.
func withBody(status int, body io.ReadCloser) httpmock.Responder {
	return httpmock.ResponderFromResponse(&http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       body,
	})
}

// stallsBody is a step answering status at once with a body that then stalls: every Read blocks
// until the attempt's context ends, the shape of a server that sent its headers and went silent.
// The body needs the request's own context, so this is a responder closure rather than a response.
func stallsBody(status int) httpmock.Responder {
	return func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: status,
			Header:     make(http.Header),
			Body:       &stalledBody{done: req.Context().Done(), cause: req.Context().Err},
			Request:    req,
		}, nil
	}
}

// rateLimited is a step answering 429 with the given Retry-After header value.
func rateLimited(retryAfter string) httpmock.Responder {
	return httpmock.NewStringResponder(http.StatusTooManyRequests, "rate limited").
		HeaderSet(http.Header{"Retry-After": {retryAfter}})
}

// newScriptedTransport returns a per-client httpmock.MockTransport that answers method testURL
// with steps, one per request, in order, and fails every request after the last step. attempts
// returns the bubble's fake time of every request that reached the transport so far.
//
// httpmock counts calls but does not timestamp them, and these cases pin the backoff schedule to
// exact per-attempt times, so every responder is wrapped to record time.Now() first. A request
// that matches no route is recorded too, through the no-responder: without one, httpmock fails
// such a request without counting it, and an extra attempt sent elsewhere would go unseen.
//
// When the request's context can end, httpmock runs the responder on a goroutine of its own and
// RoundTrip can return (with the context's error) before that goroutine has recorded its attempt.
// A case whose request context may already have ended therefore reads attempts only after
// synctest.Wait().
func newScriptedTransport(method string, steps ...httpmock.Responder) (
	transport *httpmock.MockTransport, attempts func() []time.Time,
) {
	var (
		mu sync.Mutex
		at []time.Time
	)

	recorded := func(next httpmock.Responder) httpmock.Responder {
		return func(req *http.Request) (*http.Response, error) {
			mu.Lock()
			at = append(at, time.Now())
			mu.Unlock()

			return next(req)
		}
	}

	script := steps[0]
	for _, next := range steps[1:] {
		script = script.Then(next)
	}

	script = script.Then(httpmock.NewErrorResponder(errors.New("no scripted step left")))

	transport = httpmock.NewMockTransport()
	transport.RegisterResponder(method, testURL, recorded(script))
	transport.RegisterNoResponder(recorded(httpmock.ConnectionFailure))

	return transport, func() []time.Time {
		mu.Lock()
		defer mu.Unlock()

		return slices.Clone(at)
	}
}

// fixedJitter builds a Jitter func that always returns v, so RetryTransport's backoff is exactly
// BaseDelay*2^n*(0.8+0.4*v) rather than a range. The brief pins v=0.5 to the schedule 1s, 2s, 4s.
func fixedJitter(v float64) func() float64 {
	return func() float64 { return v }
}

func newGetRequest(ctx context.Context, method string) *http.Request {
	req, err := http.NewRequestWithContext(ctx, method, testURL, http.NoBody)
	if err != nil {
		// Building a request from a constant, valid URL never fails; a failure here is a bug in
		// the test helper, not in the code under test.
		panic(err)
	}

	return req
}

// RetrySuite covers httpclient.RetryTransport (T019, R12, FR-031): which failures are retried, the
// backoff and Retry-After schedule, the retry cap, and the resource and cancellation guarantees
// around a retried attempt.
//
// Every case runs its RoundTrip call inside a testing/synctest bubble against a scripted
// httpmock transport (newScriptedTransport), so the 1s/2s/4s backoff and any Retry-After wait are
// exact and instant rather than real sleeps.
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
		attempts                            []time.Time
		start                               time.Time
		statusCode                          int
		bodyText                            string
		respErr                             error
		firstBodyDrained, secondBodyDrained bool
		finalBodyClosedByTransport          bool
	)

	synctest.Test(s.T(), func(t *testing.T) {
		bodies := []*fakeBody{newFakeBody("boom-1"), newFakeBody("boom-2"), newFakeBody("ok")}
		transport, snapshot := newScriptedTransport(
			http.MethodGet,
			withBody(http.StatusInternalServerError, bodies[0]),
			withBody(http.StatusInternalServerError, bodies[1]),
			withBody(http.StatusOK, bodies[2]),
		)
		rt := &httpclient.RetryTransport{Base: transport, Jitter: fixedJitter(0.5)}

		start = time.Now()

		resp, err := rt.RoundTrip(newGetRequest(t.Context(), http.MethodGet))
		respErr = err
		attempts = snapshot()
		firstBodyDrained = bodies[0].drainedAndClosed()
		secondBodyDrained = bodies[1].drainedAndClosed()

		if resp == nil {
			return
		}

		// Captured before this test's own read and close below, so it reflects only what
		// RetryTransport itself did with the final, returned attempt's body.
		finalBodyClosedByTransport = bodies[2].wasClosed()

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
	s.Equal(start, attempts[0], "the first attempt is immediate")
	s.Equal(start.Add(1*time.Second), attempts[1], "jitter 0.5 makes the first backoff exactly 1s")
	s.Equal(start.Add(3*time.Second), attempts[2], "second backoff is exactly 2s, so 1s+2s after start")

	s.True(firstBodyDrained, "the first 500's body must be drained and closed before retrying")
	s.True(secondBodyDrained, "the second 500's body must be drained and closed before retrying")
	s.False(finalBodyClosedByTransport, "the final, returned response body must be left for the caller to close")
	s.Equal("ok", bodyText)
}

// drainCapBytes mirrors the cap httpclient.RetryTransport documents for a discarded body's drain: a
// body far larger than this must never be read in full before being closed.
const drainCapBytes = 64 << 10

func (s *RetrySuite) TestDiscardedBodyDrainIsCappedThenClosed() {
	var (
		attempts       []time.Time
		probeBytesRead int
		probeClosed    bool
	)

	synctest.Test(s.T(), func(t *testing.T) {
		probe := &cappedProbeBody{}
		transport, snapshot := newScriptedTransport(
			http.MethodGet,
			withBody(http.StatusInternalServerError, probe),
			httpmock.NewStringResponder(http.StatusOK, "ok"),
		)
		rt := &httpclient.RetryTransport{Base: transport, Jitter: fixedJitter(0.5)}

		resp, err := rt.RoundTrip(newGetRequest(t.Context(), http.MethodGet))
		if err != nil {
			t.Fatal(err)
		}

		if resp != nil {
			_ = resp.Body.Close()
		}

		attempts = snapshot()
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
		attempts   []time.Time
		start      time.Time
		statusCode int
		respErr    error
	)

	synctest.Test(s.T(), func(t *testing.T) {
		transport, snapshot := newScriptedTransport(
			http.MethodGet,
			httpmock.NewErrorResponder(errors.New("dial tcp 203.0.113.1:443: connect: connection refused")),
			httpmock.NewStringResponder(http.StatusOK, "ok"),
		)
		rt := &httpclient.RetryTransport{Base: transport, Jitter: fixedJitter(0.5)}

		start = time.Now()

		resp, err := rt.RoundTrip(newGetRequest(t.Context(), http.MethodGet))
		respErr = err
		attempts = snapshot()

		if resp != nil {
			statusCode = resp.StatusCode

			_ = resp.Body.Close()
		}
	})

	s.Require().NoError(respErr)
	s.Equal(http.StatusOK, statusCode)
	s.Require().Len(attempts, 2)
	s.Equal(start.Add(1*time.Second), attempts[1], "a network error backs off exactly like a 5xx")
}

func (s *RetrySuite) TestHEADIsRetried() {
	var (
		attempts   []time.Time
		start      time.Time
		statusCode int
		respErr    error
	)

	synctest.Test(s.T(), func(t *testing.T) {
		transport, snapshot := newScriptedTransport(
			http.MethodHead,
			httpmock.NewStringResponder(http.StatusInternalServerError, ""),
			httpmock.NewStringResponder(http.StatusOK, ""),
		)
		rt := &httpclient.RetryTransport{Base: transport, Jitter: fixedJitter(0.5)}

		start = time.Now()

		resp, err := rt.RoundTrip(newGetRequest(t.Context(), http.MethodHead))
		respErr = err
		attempts = snapshot()

		if resp != nil {
			statusCode = resp.StatusCode

			_ = resp.Body.Close()
		}
	})

	s.Require().NoError(respErr)
	s.Equal(http.StatusOK, statusCode)
	s.Require().Len(attempts, 2, "HEAD is idempotent and must be retried like GET")
	s.Equal(start.Add(1*time.Second), attempts[1], "a HEAD retry backs off exactly like a GET retry")
}

func (s *RetrySuite) TestRetryAfterSecondsHeaderReplacesTheBackoff() {
	var (
		attempts []time.Time
		start    time.Time
		respErr  error
	)

	synctest.Test(s.T(), func(t *testing.T) {
		transport, snapshot := newScriptedTransport(
			http.MethodGet,
			rateLimited("2"),
			httpmock.NewStringResponder(http.StatusOK, "ok"),
		)
		// Jitter 0.99 would push the computed backoff toward 1.4s if it were used; pinning the
		// wait to exactly 2s proves the header overrides the computed backoff rather than adding
		// to it.
		rt := &httpclient.RetryTransport{Base: transport, Jitter: fixedJitter(0.99)}

		start = time.Now()

		resp, err := rt.RoundTrip(newGetRequest(t.Context(), http.MethodGet))
		respErr = err
		attempts = snapshot()

		if resp != nil {
			_ = resp.Body.Close()
		}
	})

	s.Require().NoError(respErr)
	s.Require().Len(attempts, 2)
	s.Equal(start.Add(2*time.Second), attempts[1], "Retry-After: 2 must wait exactly 2s, not a jittered backoff")
}

func (s *RetrySuite) TestRetryAfterHTTPDateIsHonored() {
	var (
		attempts []time.Time
		start    time.Time
		retryAt  time.Time
		respErr  error
	)

	synctest.Test(s.T(), func(t *testing.T) {
		start = time.Now()
		retryAt = start.Add(5 * time.Second)

		transport, snapshot := newScriptedTransport(
			http.MethodGet,
			rateLimited(retryAt.UTC().Format(http.TimeFormat)),
			httpmock.NewStringResponder(http.StatusOK, "ok"),
		)
		rt := &httpclient.RetryTransport{Base: transport, Jitter: fixedJitter(0.5)}

		resp, err := rt.RoundTrip(newGetRequest(t.Context(), http.MethodGet))
		respErr = err
		attempts = snapshot()

		if resp != nil {
			_ = resp.Body.Close()
		}
	})

	s.Require().NoError(respErr)
	s.Require().Len(attempts, 2)
	s.Equal(retryAt, attempts[1], "an HTTP-date Retry-After must be honored exactly")
}

func (s *RetrySuite) TestRetryAfterNegativeSecondsIsClampedToAnImmediateRetry() {
	var (
		attempts []time.Time
		start    time.Time
		respErr  error
	)

	synctest.Test(s.T(), func(t *testing.T) {
		transport, snapshot := newScriptedTransport(
			http.MethodGet,
			rateLimited("-5"),
			httpmock.NewStringResponder(http.StatusOK, "ok"),
		)
		rt := &httpclient.RetryTransport{Base: transport, Jitter: fixedJitter(0.5)}

		start = time.Now()

		resp, err := rt.RoundTrip(newGetRequest(t.Context(), http.MethodGet))
		respErr = err
		attempts = snapshot()

		if resp != nil {
			_ = resp.Body.Close()
		}
	})

	s.Require().NoError(respErr)
	s.Require().Len(attempts, 2)
	s.Equal(
		start,
		attempts[1],
		"a negative Retry-After must be clamped to an immediate retry, never a negative wait",
	)
}

func (s *RetrySuite) TestRetryAfterOverTheCapStopsAtOnceWithRetryAfterTooLongError() {
	var (
		attempts   []time.Time
		elapsed    time.Duration
		respWasNil bool
		respErr    error
	)

	synctest.Test(s.T(), func(t *testing.T) {
		transport, snapshot := newScriptedTransport(http.MethodGet, rateLimited("120"))
		rt := &httpclient.RetryTransport{Base: transport, Jitter: fixedJitter(0.5), MaxWait: 60 * time.Second}

		start := time.Now()

		resp, err := rt.RoundTrip(newGetRequest(t.Context(), http.MethodGet))
		respErr = err
		elapsed = time.Since(start)
		attempts = snapshot()
		respWasNil = resp == nil

		if resp != nil {
			_ = resp.Body.Close()
		}
	})

	s.Require().Len(attempts, 1, "a Retry-After over the cap must stop before a second attempt")
	s.Equal(time.Duration(0), elapsed, "a wait longer than the cap must never be waited out, even partially")
	s.True(respWasNil, "a Retry-After over the cap must return no response, only the error")

	s.Require().Error(respErr)

	var tooLong *httpclient.RetryAfterTooLongError
	s.Require().ErrorAs(respErr, &tooLong)
	s.Equal(http.StatusTooManyRequests, tooLong.Status)
	s.Equal(120*time.Second, tooLong.RetryAfter)
	s.Contains(tooLong.Error(), strconv.Itoa(http.StatusTooManyRequests))
	s.Contains(tooLong.Error(), (120 * time.Second).String())
}

// TestRetryAfterOverTheCapOnTheFinalAttemptStillFails pins the order of the two checks on the last
// attempt: the Retry-After cap runs before the final attempt hands its response back, so an over-cap
// wait on the final attempt still fails with *RetryAfterTooLongError and returns no response, and
// every body is closed, the over-cap one included.
func (s *RetrySuite) TestRetryAfterOverTheCapOnTheFinalAttemptStillFails() {
	var (
		attempts   []time.Time
		respWasNil bool
		respErr    error
		drained    []bool
	)

	synctest.Test(s.T(), func(t *testing.T) {
		bodies := []*fakeBody{
			newFakeBody("boom-1"), newFakeBody("boom-2"), newFakeBody("boom-3"), newFakeBody("rate limited"),
		}
		transport, snapshot := newScriptedTransport(
			http.MethodGet,
			withBody(http.StatusInternalServerError, bodies[0]),
			withBody(http.StatusInternalServerError, bodies[1]),
			withBody(http.StatusInternalServerError, bodies[2]),
			httpmock.ResponderFromResponse(&http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     http.Header{"Retry-After": {"120"}},
				Body:       bodies[3],
			}),
		)
		rt := &httpclient.RetryTransport{Base: transport, Jitter: fixedJitter(0.5), MaxWait: 60 * time.Second}

		resp, err := rt.RoundTrip(newGetRequest(t.Context(), http.MethodGet))
		respErr = err
		attempts = snapshot()
		respWasNil = resp == nil

		for _, body := range bodies {
			drained = append(drained, body.drainedAndClosed())
		}

		if resp != nil {
			_ = resp.Body.Close()
		}
	})

	s.Require().Len(attempts, 4, "the over-cap Retry-After arrives on the fourth and final attempt")
	s.True(respWasNil, "a Retry-After over the cap must return no response, even on the final attempt")

	var tooLong *httpclient.RetryAfterTooLongError
	s.Require().ErrorAs(respErr, &tooLong)
	s.Equal(http.StatusTooManyRequests, tooLong.Status)
	s.Equal(120*time.Second, tooLong.RetryAfter)

	s.Equal([]bool{true, true, true, true}, drained, "every body must be drained and closed, the over-cap one included")
}

func (s *RetrySuite) TestClientErrorStatusesAreNeverRetried() {
	statuses := []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound}

	for _, status := range statuses {
		s.Run(strconv.Itoa(status), func() {
			var (
				attempts   []time.Time
				statusCode int
				respErr    error
			)

			synctest.Test(s.T(), func(t *testing.T) {
				transport, snapshot := newScriptedTransport(
					http.MethodGet,
					httpmock.NewStringResponder(status, "client error"),
					httpmock.NewStringResponder(http.StatusOK, "unreachable if retried"),
				)
				rt := &httpclient.RetryTransport{Base: transport, Jitter: fixedJitter(0.5)}

				resp, err := rt.RoundTrip(newGetRequest(t.Context(), http.MethodGet))
				respErr = err
				attempts = snapshot()

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
		attempts   []time.Time
		statusCode int
		respErr    error
	)

	synctest.Test(s.T(), func(t *testing.T) {
		transport, snapshot := newScriptedTransport(
			http.MethodPost,
			httpmock.NewStringResponder(http.StatusInternalServerError, "boom"),
			httpmock.NewStringResponder(http.StatusOK, "unreachable if retried"),
		)
		rt := &httpclient.RetryTransport{Base: transport, Jitter: fixedJitter(0.5)}

		resp, err := rt.RoundTrip(newGetRequest(t.Context(), http.MethodPost))
		respErr = err
		attempts = snapshot()

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
		transport, _ := newScriptedTransport(
			http.MethodGet,
			httpmock.NewStringResponder(http.StatusInternalServerError, "boom-1"),
			httpmock.NewStringResponder(http.StatusInternalServerError, "boom-2"),
			httpmock.NewStringResponder(http.StatusOK, "ok"),
		)
		rt := &httpclient.RetryTransport{Base: transport, Jitter: fixedJitter(0.5)}

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

// TestContextEndedBeforeTheFirstAttemptMakesNoAttempt pins what retry-go does with a request
// whose context has already ended: it checks the context before the first attempt, so nothing is
// sent, no response comes back, and the error still unwraps to the context's own.
func (s *RetrySuite) TestContextEndedBeforeTheFirstAttemptMakesNoAttempt() {
	var (
		attempts   []time.Time
		respWasNil bool
		respErr    error
	)

	synctest.Test(s.T(), func(t *testing.T) {
		transport, snapshot := newScriptedTransport(
			http.MethodGet,
			httpmock.NewStringResponder(http.StatusInternalServerError, "unreachable, the context ended first"),
			httpmock.NewStringResponder(http.StatusOK, "unreachable, the context ended first"),
		)
		rt := &httpclient.RetryTransport{Base: transport, Jitter: fixedJitter(0.5)}

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		resp, err := rt.RoundTrip(newGetRequest(ctx, http.MethodGet))
		respErr = err
		respWasNil = resp == nil

		// httpmock runs a responder on a goroutine of its own whenever the request's context can
		// end, and returns as soon as the context has ended, possibly before that goroutine has
		// recorded the attempt. Waiting for the bubble to settle keeps an attempt made anyway from
		// going unseen.
		synctest.Wait()

		attempts = snapshot()

		if resp != nil {
			_ = resp.Body.Close()
		}
	})

	s.Empty(attempts, "a request whose context already ended must never be sent")
	s.True(respWasNil, "a request whose context already ended must return no response")
	s.Require().ErrorIs(respErr, context.Canceled)
	s.Contains(respErr.Error(), "httpclient: request context ended")
}

func (s *RetrySuite) TestGivesUpAfterMaxRetriesReturningTheLastResponse() {
	var (
		attempts   []time.Time
		statusCode int
		bodyText   string
		respErr    error
	)

	synctest.Test(s.T(), func(t *testing.T) {
		transport, snapshot := newScriptedTransport(
			http.MethodGet,
			httpmock.NewStringResponder(http.StatusInternalServerError, "boom-1"),
			httpmock.NewStringResponder(http.StatusInternalServerError, "boom-2"),
			httpmock.NewStringResponder(http.StatusInternalServerError, "boom-3"),
			httpmock.NewStringResponder(http.StatusInternalServerError, "boom-4"),
			httpmock.NewStringResponder(
				http.StatusInternalServerError,
				"unreachable, a 5th attempt is one retry too many",
			),
		)
		rt := &httpclient.RetryTransport{Base: transport, Jitter: fixedJitter(0.5)}

		resp, err := rt.RoundTrip(newGetRequest(t.Context(), http.MethodGet))
		respErr = err
		attempts = snapshot()

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
		attempts []time.Time
		respErr  error
	)

	synctest.Test(s.T(), func(t *testing.T) {
		transport, snapshot := newScriptedTransport(
			http.MethodGet,
			httpmock.NewErrorResponder(errors.New("boom-1: connection reset")),
			httpmock.NewErrorResponder(errors.New("boom-2: connection reset")),
			httpmock.NewErrorResponder(errors.New("boom-3: connection reset")),
			httpmock.NewErrorResponder(lastErr),
		)
		rt := &httpclient.RetryTransport{Base: transport, Jitter: fixedJitter(0.5)}

		resp, err := rt.RoundTrip(newGetRequest(t.Context(), http.MethodGet))
		respErr = err
		attempts = snapshot()

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
	var attempts []time.Time

	synctest.Test(s.T(), func(t *testing.T) {
		transport, snapshot := newScriptedTransport(
			http.MethodGet,
			httpmock.NewStringResponder(http.StatusInternalServerError, "boom-1"),
			httpmock.NewStringResponder(http.StatusInternalServerError, "boom-2"),
			httpmock.NewStringResponder(http.StatusOK, "ok"),
		)
		rt := &httpclient.RetryTransport{Base: transport}

		resp, err := rt.RoundTrip(newGetRequest(t.Context(), http.MethodGet))
		if err != nil {
			t.Fatal(err)
		}

		if resp != nil {
			_ = resp.Body.Close()
		}

		attempts = snapshot()
	})

	s.Require().Len(attempts, 3)

	firstWait := attempts[1].Sub(attempts[0])
	secondWait := attempts[2].Sub(attempts[1])
	s.InDelta(1*time.Second, firstWait, float64(200*time.Millisecond), "default BaseDelay is 1s +/- 20%% jitter")
	s.InDelta(2*time.Second, secondWait, float64(400*time.Millisecond), "default backoff doubles to 2s +/- 20%% jitter")
}

// TestGivesUpAfterMaxRetriesErrorCarriesOnlyTheLastAttempt pins that giving up reports the last
// attempt's error alone, never a concatenation of every attempt's error: the text reaches the log
// file and an unchecked account's reason, where four near-identical failures would only be noise.
func (s *RetrySuite) TestGivesUpAfterMaxRetriesErrorCarriesOnlyTheLastAttempt() {
	var respErr error

	synctest.Test(s.T(), func(t *testing.T) {
		transport, _ := newScriptedTransport(
			http.MethodGet,
			httpmock.NewErrorResponder(errors.New("boom-1: connection reset")),
			httpmock.NewErrorResponder(errors.New("boom-2: connection reset")),
			httpmock.NewErrorResponder(errors.New("boom-3: connection reset")),
			httpmock.NewErrorResponder(errors.New("boom-4: connection reset")),
		)
		rt := &httpclient.RetryTransport{Base: transport, Jitter: fixedJitter(0.5)}

		resp, err := rt.RoundTrip(newGetRequest(t.Context(), http.MethodGet))
		respErr = err

		if resp != nil {
			_ = resp.Body.Close()
		}
	})

	s.Require().Error(respErr)
	s.Contains(respErr.Error(), "boom-4")

	for _, earlier := range []string{"boom-1", "boom-2", "boom-3"} {
		s.NotContains(respErr.Error(), earlier, "an earlier attempt's error must not be carried into the final one")
	}
}

// TestRetryingWritesNothingToStderr pins invariant 4 (quiet when clean) for the retry layer: cron
// mails any stderr output, so retrying, giving up and refusing an over-cap Retry-After must never
// print anything, whether from a retry helper's own logging or from net/http's global logger,
// which complains when a RoundTripper returns both a response and an error. Each case runs a
// request through NewClient with os.Stderr and the standard logger pointed at a file, then asserts
// the file stayed empty. The file is read only after the bubble ends: a pipe read inside the bubble
// would block the fake clock.
func (s *RetrySuite) TestRetryingWritesNothingToStderr() {
	cases := []struct {
		name  string
		steps []httpmock.Responder
	}{
		{name: "retried then succeeds", steps: []httpmock.Responder{
			httpmock.NewStringResponder(http.StatusInternalServerError, "boom"),
			httpmock.NewErrorResponder(errors.New("connection reset")),
			rateLimited("1"),
			httpmock.NewStringResponder(http.StatusOK, "ok"),
		}},
		{name: "gives up returning the last response", steps: []httpmock.Responder{
			httpmock.NewStringResponder(http.StatusServiceUnavailable, "down-1"),
			httpmock.NewStringResponder(http.StatusServiceUnavailable, "down-2"),
			httpmock.NewStringResponder(http.StatusServiceUnavailable, "down-3"),
			httpmock.NewStringResponder(http.StatusServiceUnavailable, "down-4"),
		}},
		{name: "gives up returning the last error", steps: []httpmock.Responder{
			httpmock.NewErrorResponder(errors.New("reset-1")),
			httpmock.NewErrorResponder(errors.New("reset-2")),
			httpmock.NewErrorResponder(errors.New("reset-3")),
			httpmock.NewErrorResponder(errors.New("reset-4")),
		}},
		{name: "retry-after over the cap", steps: []httpmock.Responder{
			rateLimited("120"),
		}},
	}

	for _, tc := range cases {
		s.Run(tc.name, func() {
			s.Empty(s.captureStderr(func() {
				synctest.Test(s.T(), func(t *testing.T) {
					transport, _ := newScriptedTransport(http.MethodGet, tc.steps...)
					client := httpclient.NewClient(
						&httpclient.RetryTransport{Base: transport, Jitter: fixedJitter(0.5)},
					)

					resp, err := client.Do(newGetRequest(t.Context(), http.MethodGet))
					if err == nil {
						_ = resp.Body.Close()
					}
				})
			}), "retrying must write nothing to stderr")
		})
	}
}

// captureStderr runs fn with os.Stderr and the standard logger's output both redirected to a
// file, restores them, and returns whatever fn wrote.
func (s *RetrySuite) captureStderr(fn func()) string {
	file, err := os.CreateTemp(s.T().TempDir(), "stderr")
	s.Require().NoError(err)

	defer func() { _ = file.Close() }()

	origStderr, origLog := os.Stderr, log.Writer()
	os.Stderr = file
	log.SetOutput(file)

	func() {
		defer func() {
			os.Stderr = origStderr
			log.SetOutput(origLog)
		}()

		fn()
	}()

	data, err := os.ReadFile(file.Name())
	s.Require().NoError(err)

	return string(data)
}
