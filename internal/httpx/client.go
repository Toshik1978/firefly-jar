package httpx

import (
	"net/http"
)

// NewClient builds an *http.Client using rt as its transport. It never follows a redirect
// automatically: CheckRedirect returns http.ErrUseLastResponse so a caller decides explicitly,
// which keeps a bearer token configured for one host from being replayed to a redirect target
// (R8, R12).
//
// The client sets no Timeout on purpose: http.Client.Timeout spans the whole exchange, every retry
// and Retry-After wait inside rt included, so a 45 s Retry-After (under R12's 60 s cap) would be
// cut short. rt bounds each attempt instead (RetryTransport and TimeoutTransport, 30 s each), and
// the caller's context bounds the exchange (the 10-minute run cap).
func NewClient(rt http.RoundTripper) *http.Client {
	return &http.Client{
		Transport: rt,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
