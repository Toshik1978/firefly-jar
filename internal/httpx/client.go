package httpx

import (
	"net/http"
	"time"
)

// clientTimeout bounds every request an *http.Client built by NewClient makes, so a hung
// connection can never block a whole cron run (R12).
const clientTimeout = 30 * time.Second

// NewClient builds an *http.Client using rt as its transport. It never follows a redirect
// automatically: CheckRedirect returns http.ErrUseLastResponse so a caller decides explicitly,
// which keeps a bearer token configured for one host from being replayed to a redirect target
// (R8, R12).
func NewClient(rt http.RoundTripper) *http.Client {
	return &http.Client{
		Transport: rt,
		Timeout:   clientTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
