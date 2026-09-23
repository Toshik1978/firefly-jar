package httpx_test

import (
	"context"
	"io"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/suite"

	"github.com/Toshik1978/firefly-jar/internal/httpx"
)

// ClientSuite covers httpx.NewClient (T019, R8, R12, FR-031): no automatic redirect handling, so a
// bearer token configured for one host is never replayed to another after a 3xx, and timeouts that
// bound each attempt rather than the whole exchange, so a Retry-After wait or a retry after a hung
// attempt is never cut short by a clock that started before the first attempt. The synctest cases
// follow RetrySuite's conventions: collect plain values inside the bubble, assert after it.
type ClientSuite struct {
	suite.Suite
}

func (s *ClientSuite) TestNewClientHasNoWholeExchangeTimeout() {
	client := httpx.NewClient(http.DefaultTransport)

	s.Zero(client.Timeout, "a client-wide timeout would span every retry and Retry-After wait (R12)")
}

func (s *ClientSuite) TestNewClientNeverFollowsARedirect() {
	client := httpx.NewClient(http.DefaultTransport)
	s.Require().NotNil(client.CheckRedirect, "a nil CheckRedirect would follow redirects automatically")

	err := client.CheckRedirect(&http.Request{}, nil)

	s.Require().ErrorIs(err, http.ErrUseLastResponse)
}

// TestRetryAfterLongerThanOneAttemptTimeoutStillSucceeds asserts a 429 with Retry-After: 45 is
// waited out and retried through a client built by NewClient: 45 s is under the 60 s Retry-After
// cap, so R12 says wait, and a 30 s timeout spanning the whole exchange would cancel the wait.
func (s *ClientSuite) TestRetryAfterLongerThanOneAttemptTimeoutStillSucceeds() {
	var (
		attempts   []fakeAttempt
		start      time.Time
		statusCode int
		respErr    error
	)

	synctest.Test(s.T(), func(t *testing.T) {
		header := make(http.Header)
		header.Set("Retry-After", "45")
		fake := &fakeTransport{steps: []scriptedStep{
			{status: http.StatusTooManyRequests, header: header, body: "rate limited"},
			{status: http.StatusOK, body: "ok"},
		}}
		client := httpx.NewClient(&httpx.RetryTransport{Base: fake, Jitter: fixedJitter(0.5)})

		start = time.Now()

		resp, err := client.Do(newGetRequest(t.Context(), http.MethodGet))
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
	s.Equal(start.Add(45*time.Second), attempts[1].at, "Retry-After: 45 must be waited out in full")
}

// TestHungAttemptIsBoundedAndThenRetried asserts one attempt that never answers is cut off after
// the 30 s per-attempt timeout and then retried after the normal backoff, instead of hanging the
// run or failing the whole exchange.
func (s *ClientSuite) TestHungAttemptIsBoundedAndThenRetried() {
	var (
		attempts   []fakeAttempt
		start      time.Time
		statusCode int
		respErr    error
	)

	synctest.Test(s.T(), func(t *testing.T) {
		fake := &fakeTransport{steps: []scriptedStep{
			{hang: true},
			{status: http.StatusOK, body: "ok"},
		}}
		client := httpx.NewClient(&httpx.RetryTransport{Base: fake, Jitter: fixedJitter(0.5)})

		start = time.Now()

		resp, err := client.Do(newGetRequest(t.Context(), http.MethodGet))
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
	s.Equal(start.Add(31*time.Second), attempts[1].at, "a 30 s attempt timeout, then the 1 s first backoff")
}

// TestHungNonRetriedRequestIsBounded asserts a request the retry layer never replays (a POST) is
// still bounded by the per-attempt timeout, so a hung auth call cannot hold the run until the
// 10-minute run cap.
func (s *ClientSuite) TestHungNonRetriedRequestIsBounded() {
	var (
		attempts []fakeAttempt
		elapsed  time.Duration
		respErr  error
	)

	synctest.Test(s.T(), func(t *testing.T) {
		fake := &fakeTransport{steps: []scriptedStep{{hang: true}}}
		client := httpx.NewClient(&httpx.RetryTransport{Base: fake})

		start := time.Now()

		resp, err := client.Do(newGetRequest(t.Context(), http.MethodPost))
		respErr = err
		attempts = fake.attemptSnapshot()
		elapsed = time.Since(start)

		if resp != nil {
			_ = resp.Body.Close()
		}
	})

	s.Require().ErrorIs(respErr, context.DeadlineExceeded)
	s.Len(attempts, 1, "a POST is never retried")
	s.Equal(30*time.Second, elapsed)
}

// TestTimeoutTransportBoundsAHungRequestWithoutRetrying asserts the bare per-attempt layer, the
// one Telegram uses because it handles its own 429s, cuts a hung request off at 30 s.
func (s *ClientSuite) TestTimeoutTransportBoundsAHungRequestWithoutRetrying() {
	var (
		attempts []fakeAttempt
		elapsed  time.Duration
		respErr  error
	)

	synctest.Test(s.T(), func(t *testing.T) {
		fake := &fakeTransport{steps: []scriptedStep{{hang: true}, {status: http.StatusOK}}}
		client := httpx.NewClient(&httpx.TimeoutTransport{Base: fake})

		start := time.Now()

		resp, err := client.Do(newGetRequest(t.Context(), http.MethodGet))
		respErr = err
		attempts = fake.attemptSnapshot()
		elapsed = time.Since(start)

		if resp != nil {
			_ = resp.Body.Close()
		}
	})

	s.Require().ErrorIs(respErr, context.DeadlineExceeded)
	s.Len(attempts, 1, "TimeoutTransport never retries")
	s.Equal(30*time.Second, elapsed)
}

// TestAttemptTimeoutCoversTheBodyRead asserts the per-attempt deadline stays armed after the
// headers arrive, so a server that sends its headers and then stalls the body is cut off 30 s after
// the attempt started: the attempt deadline is the body-read cap too.
func (s *ClientSuite) TestAttemptTimeoutCoversTheBodyRead() {
	var (
		elapsed time.Duration
		readErr error
	)

	synctest.Test(s.T(), func(t *testing.T) {
		fake := &fakeTransport{steps: []scriptedStep{{status: http.StatusOK, stallBody: true}}}
		client := httpx.NewClient(&httpx.RetryTransport{Base: fake})

		start := time.Now()

		resp, err := client.Do(newGetRequest(t.Context(), http.MethodGet))
		if err != nil {
			t.Fatal(err)
		}

		defer func() { _ = resp.Body.Close() }()

		_, readErr = io.ReadAll(resp.Body)
		elapsed = time.Since(start)
	})

	s.Require().ErrorIs(readErr, context.DeadlineExceeded)
	s.Equal(30*time.Second, elapsed)
}
