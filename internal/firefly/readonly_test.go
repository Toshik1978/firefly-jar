package firefly

import (
	"context"
	"net/http"
	"net/url"

	"github.com/jarcoal/httpmock"
	"github.com/stretchr/testify/suite"
)

// fireflyTestURL is the fixed, unreachable base URL every suite in this package registers its
// httpmock responders against and passes to New. Requests never leave the process: httpmock
// intercepts them at the http.RoundTripper each client is built with (per-client
// httpmock.NewMockTransport, never the global DefaultTransport), so the exact host is arbitrary and
// shared across suites for consistency.
const fireflyTestURL = "http://firefly.test"

// ReadOnlySuite covers the Firefly III read-only transport guard (T031, FR-001, constitution §I,
// research R8): every write method is rejected by ReadOnlyTransport before the request leaves the
// process -- both in isolation and as wired into a client built by New -- GET passes
// through, a redirect is never followed, and the unexported (*Client).get helper sends the headers
// R8 requires. It is the RED half of T032: every case here fails today for lack of a production
// ReadOnlyTransport, ErrWriteForbidden, Client, New, and get.
type ReadOnlySuite struct {
	suite.Suite
}

// rejectedMethods lists every HTTP method the transport must reject, per FR-001 and constitution §I
// ("MUST issue only GET requests"): the four write verbs, every other standard method, and a
// made-up extension method, so the guard is an allow-list of one rather than a deny-list.
func (s *ReadOnlySuite) rejectedMethods() []string {
	return []string{
		http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete,
		http.MethodHead, http.MethodOptions, http.MethodTrace, http.MethodConnect,
		"PROPFIND",
	}
}

// readMethods lists every HTTP method the transport must pass through unchanged: GET only.
func (s *ReadOnlySuite) readMethods() []string {
	return []string{http.MethodGet}
}

// mockTransport returns an httpmock.MockTransport whose only registered responder answers GET
// fireflyTestURL with status, so a test can assert exactly how many requests actually reached it
// via GetTotalCallCount -- a blocked write method must never reach it at all, since ReadOnlyTransport
// rejects it before Base.RoundTrip is ever called.
//
// RegisterNoResponder is required for that proof to mean anything: httpmock counts a request only
// once it reaches a responder, and an unmatched request with no NoResponder registered fails with
// ConnectionFailure without being counted at all, so GetTotalCallCount would read 0 whether or not
// the write method actually reached this transport -- a write method leaking past ReadOnlyTransport
// has no registered responder either (only GET is registered above), so without a NoResponder the
// leak would go uncounted and the assertion would be vacuously true.
// Registering ConnectionFailure (httpmock's own default) as the NoResponder keeps the response
// behavior identical while making every request that reaches the transport counted, matched or not.
func (s *ReadOnlySuite) mockTransport(status int) *httpmock.MockTransport {
	transport := httpmock.NewMockTransport()
	transport.RegisterResponder(http.MethodGet, fireflyTestURL+"/", httpmock.NewStringResponder(status, ""))
	transport.RegisterNoResponder(httpmock.ConnectionFailure)

	return transport
}

// TestRoundTripBlocksWriteMethodsDirectly asserts that calling ReadOnlyTransport.RoundTrip
// directly, without going through an *http.Client, still rejects every write method with
// ErrWriteForbidden and never lets the request reach the server (hit counter stays 0).
func (s *ReadOnlySuite) TestRoundTripBlocksWriteMethodsDirectly() {
	for _, method := range s.rejectedMethods() {
		s.Run(method, func() {
			transport := s.mockTransport(http.StatusOK)
			roTransport := &ReadOnlyTransport{Base: transport}

			req, err := http.NewRequestWithContext(context.Background(), method, fireflyTestURL+"/", http.NoBody)
			s.Require().NoError(err)

			resp, err := roTransport.RoundTrip(req)
			if resp != nil {
				s.Require().NoError(resp.Body.Close())
			}

			s.Require().Error(err)
			s.Require().ErrorIs(err, ErrWriteForbidden)
			s.Equal(0, transport.GetTotalCallCount(), "a blocked write method must never reach the server")
		})
	}
}

// TestRoundTripBlocksWriteMethodsThroughHTTPClient asserts the same rejection when
// ReadOnlyTransport is wired as an *http.Client's Transport, the shape a real caller uses.
func (s *ReadOnlySuite) TestRoundTripBlocksWriteMethodsThroughHTTPClient() {
	for _, method := range s.rejectedMethods() {
		s.Run(method, func() {
			transport := s.mockTransport(http.StatusOK)
			client := &http.Client{Transport: &ReadOnlyTransport{Base: transport}}

			req, err := http.NewRequestWithContext(context.Background(), method, fireflyTestURL+"/", http.NoBody)
			s.Require().NoError(err)

			resp, err := client.Do(req)
			if resp != nil {
				s.Require().NoError(resp.Body.Close())
			}

			s.Require().Error(err)
			s.Require().ErrorIs(err, ErrWriteForbidden)
			s.Equal(0, transport.GetTotalCallCount(), "a blocked write method must never reach the server")
		})
	}
}

// TestClientRejectsWriteMethodsThroughItsOwnHTTPClient asserts New actually wires the read-only
// guard into the composed client's own transport chain (Invariant 1: what the composed client can
// reach, not just a bare ReadOnlyTransport in isolation). It reaches into the unexported hc field
// T032's Client must provide, and sends each write method through it exactly as a real caller
// would use the client built by New, never constructing a ReadOnlyTransport by hand.
func (s *ReadOnlySuite) TestClientRejectsWriteMethodsThroughItsOwnHTTPClient() {
	for _, method := range s.rejectedMethods() {
		s.Run(method, func() {
			transport := s.mockTransport(http.StatusOK)
			client := New(fireflyTestURL, "tok", transport)

			req, err := http.NewRequestWithContext(context.Background(), method, fireflyTestURL+"/", http.NoBody)
			s.Require().NoError(err)

			resp, err := client.hc.Do(req)
			if resp != nil {
				s.Require().NoError(resp.Body.Close())
			}

			s.Require().Error(err)
			s.Require().ErrorIs(err, ErrWriteForbidden)
			s.Equal(
				0,
				transport.GetTotalCallCount(),
				"New must wire the read-only guard so a write method never reaches the server",
			)
		})
	}
}

// TestRoundTripPassesThroughReadMethods asserts GET reaches the server unchanged: the hit
// counter increments and the server's response comes back.
func (s *ReadOnlySuite) TestRoundTripPassesThroughReadMethods() {
	for _, method := range s.readMethods() {
		s.Run(method, func() {
			transport := s.mockTransport(http.StatusOK)
			client := &http.Client{Transport: &ReadOnlyTransport{Base: transport}}

			req, err := http.NewRequestWithContext(context.Background(), method, fireflyTestURL+"/", http.NoBody)
			s.Require().NoError(err)

			resp, err := client.Do(req)
			s.Require().NoError(err)
			s.Require().NoError(resp.Body.Close())

			s.Equal(http.StatusOK, resp.StatusCode)
			s.Equal(1, transport.GetTotalCallCount(), "a read method must reach the server exactly once")
		})
	}
}

// TestGetDoesNotFollowRedirect asserts (*Client).get treats a 3xx response as an error instead of
// following it (research R8: "a 3xx is treated as a Firefly error"), so a bearer token configured
// for the first host is never replayed to a redirect target. The redirect target's hit counter
// must stay 0.
func (s *ReadOnlySuite) TestGetDoesNotFollowRedirect() {
	const targetURL = "http://firefly-redirect.test/"

	transport := httpmock.NewMockTransport()

	redirect := httpmock.NewStringResponder(http.StatusFound, "").HeaderSet(http.Header{"Location": {targetURL}})
	transport.RegisterResponder(http.MethodGet, fireflyTestURL+"/", redirect)
	transport.RegisterResponder(http.MethodGet, targetURL, httpmock.NewStringResponder(http.StatusOK, ""))

	client := New(fireflyTestURL, "token", transport)

	resp, err := client.get(context.Background(), "/", nil)
	if resp != nil {
		s.Require().NoError(resp.Body.Close())
	}

	s.Require().Error(err, "a 3xx response must be treated as an error, never followed")
	s.Equal(
		0,
		transport.GetCallCountInfo()[http.MethodGet+" "+targetURL],
		"the redirect target must never be hit",
	)
}

// TestGetSendsBearerAndAcceptHeadersAndTheRequestedPath asserts (*Client).get sends
// Authorization: Bearer <token> with the token trimmed of surrounding whitespace and newlines, and
// Accept: application/json (research R8), and that the request reaches baseURL+path carrying the
// given query.
func (s *ReadOnlySuite) TestGetSendsBearerAndAcceptHeadersAndTheRequestedPath() {
	var (
		gotMethod string
		gotPath   string
		gotQuery  url.Values
		gotHeader http.Header
	)

	transport := httpmock.NewMockTransport()
	transport.RegisterResponder(http.MethodGet, fireflyTestURL+"/accounts",
		func(req *http.Request) (*http.Response, error) {
			gotMethod = req.Method
			gotPath = req.URL.Path
			gotQuery = req.URL.Query()
			gotHeader = req.Header.Clone()

			return httpmock.NewStringResponse(http.StatusOK, ""), nil
		},
	)

	client := New(fireflyTestURL, "  tok\n", transport)

	resp, err := client.get(context.Background(), "/accounts", url.Values{"page": {"2"}})
	s.Require().NoError(err)
	s.Require().NoError(resp.Body.Close())

	s.Equal(http.MethodGet, gotMethod)
	s.Equal("/accounts", gotPath)
	s.Equal("2", gotQuery.Get("page"))
	s.Equal(
		"Bearer tok",
		gotHeader.Get("Authorization"),
		"the token must be trimmed of surrounding whitespace and newlines",
	)
	s.Equal("application/json", gotHeader.Get("Accept"))
}
