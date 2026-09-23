package firefly

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"

	"github.com/stretchr/testify/suite"
)

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

// countingServer starts an httptest.Server that answers every request with status and counts how
// many requests actually reached it, so a test can assert a blocked request never left the process.
func (s *ReadOnlySuite) countingServer(status int) (*httptest.Server, *atomic.Int32) {
	var hits atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(status)
	}))

	return server, &hits
}

// TestRoundTripBlocksWriteMethodsDirectly asserts that calling ReadOnlyTransport.RoundTrip
// directly, without going through an *http.Client, still rejects every write method with
// ErrWriteForbidden and never lets the request reach the server (hit counter stays 0).
func (s *ReadOnlySuite) TestRoundTripBlocksWriteMethodsDirectly() {
	for _, method := range s.rejectedMethods() {
		s.Run(method, func() {
			server, hits := s.countingServer(http.StatusOK)
			defer server.Close()

			transport := &ReadOnlyTransport{Base: http.DefaultTransport}

			req, err := http.NewRequestWithContext(context.Background(), method, server.URL, http.NoBody)
			s.Require().NoError(err)

			resp, err := transport.RoundTrip(req)
			if resp != nil {
				s.Require().NoError(resp.Body.Close())
			}

			s.Require().Error(err)
			s.Require().ErrorIs(err, ErrWriteForbidden)
			s.Equal(int32(0), hits.Load(), "a blocked write method must never reach the server")
		})
	}
}

// TestRoundTripBlocksWriteMethodsThroughHTTPClient asserts the same rejection when
// ReadOnlyTransport is wired as an *http.Client's Transport, the shape a real caller uses.
func (s *ReadOnlySuite) TestRoundTripBlocksWriteMethodsThroughHTTPClient() {
	for _, method := range s.rejectedMethods() {
		s.Run(method, func() {
			server, hits := s.countingServer(http.StatusOK)
			defer server.Close()

			client := &http.Client{Transport: &ReadOnlyTransport{Base: http.DefaultTransport}}

			req, err := http.NewRequestWithContext(context.Background(), method, server.URL, http.NoBody)
			s.Require().NoError(err)

			resp, err := client.Do(req)
			if resp != nil {
				s.Require().NoError(resp.Body.Close())
			}

			s.Require().Error(err)
			s.Require().ErrorIs(err, ErrWriteForbidden)
			s.Equal(int32(0), hits.Load(), "a blocked write method must never reach the server")
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
			server, hits := s.countingServer(http.StatusOK)
			defer server.Close()

			client := New(server.URL, "tok", http.DefaultTransport)

			req, err := http.NewRequestWithContext(context.Background(), method, server.URL, http.NoBody)
			s.Require().NoError(err)

			resp, err := client.hc.Do(req)
			if resp != nil {
				s.Require().NoError(resp.Body.Close())
			}

			s.Require().Error(err)
			s.Require().ErrorIs(err, ErrWriteForbidden)
			s.Equal(
				int32(0),
				hits.Load(),
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
			server, hits := s.countingServer(http.StatusOK)
			defer server.Close()

			client := &http.Client{Transport: &ReadOnlyTransport{Base: http.DefaultTransport}}

			req, err := http.NewRequestWithContext(context.Background(), method, server.URL, http.NoBody)
			s.Require().NoError(err)

			resp, err := client.Do(req)
			s.Require().NoError(err)
			s.Require().NoError(resp.Body.Close())

			s.Equal(http.StatusOK, resp.StatusCode)
			s.Equal(int32(1), hits.Load(), "a read method must reach the server exactly once")
		})
	}
}

// TestGetDoesNotFollowRedirect asserts (*Client).get treats a 3xx response as an error instead of
// following it (research R8: "a 3xx is treated as a Firefly error"), so a bearer token configured
// for the first host is never replayed to a redirect target. The redirect target's hit counter
// must stay 0.
func (s *ReadOnlySuite) TestGetDoesNotFollowRedirect() {
	serverB, hitsB := s.countingServer(http.StatusOK)
	defer serverB.Close()

	serverA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, serverB.URL, http.StatusFound)
	}))
	defer serverA.Close()

	client := New(serverA.URL, "token", http.DefaultTransport)

	resp, err := client.get(context.Background(), "/", nil)
	if resp != nil {
		s.Require().NoError(resp.Body.Close())
	}

	s.Require().Error(err, "a 3xx response must be treated as an error, never followed")
	s.Equal(int32(0), hitsB.Load(), "the redirect target must never be hit")
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

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.Query()
		gotHeader = r.Header.Clone()

		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := New(server.URL, "  tok\n", http.DefaultTransport)

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
