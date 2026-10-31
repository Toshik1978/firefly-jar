package telegram

import (
	"net/http"
	"time"
)

// Verdict is the test-visible shape of an outcome, so the suite can assert on classify without
// the package exporting it.
type Verdict struct {
	Err            error
	Wait           time.Duration
	Retry          bool
	ThreadNotFound bool
}

// Classify exposes classify to the external test package.
func Classify(resp *http.Response, attemptNo int) Verdict {
	o := classify(resp, attemptNo)

	return Verdict{Err: o.err, Wait: o.wait, Retry: o.retry, ThreadNotFound: o.threadNotFound}
}
