package firefly

import (
	"errors"
	"fmt"
	"net/http"
)

// ErrWriteForbidden is wrapped by every error ReadOnlyTransport returns when a request uses a
// method other than GET or HEAD (FR-001, constitution §I). A Firefly III personal access token is
// full read/write, so the guard lives in the transport rather than in caller discipline: no method
// but GET or HEAD ever reaches Base.
var ErrWriteForbidden = errors.New("firefly: write method forbidden")

// ReadOnlyTransport wraps Base and rejects every request whose method is not GET or HEAD before it
// reaches Base, so a write request can never leave the process.
type ReadOnlyTransport struct {
	// Base is the underlying transport. A nil Base uses http.DefaultTransport.
	Base http.RoundTripper
}

// RoundTrip implements http.RoundTripper. A method other than GET or HEAD is rejected with an
// error wrapping ErrWriteForbidden that names only the method, and req's body is closed first so
// nothing is leaked; the request never reaches Base. GET and HEAD are forwarded to Base unchanged.
func (t *ReadOnlyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		if req.Body != nil {
			_ = req.Body.Close()
		}

		return nil, fmt.Errorf("%w: %s", ErrWriteForbidden, req.Method)
	}

	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}

	resp, err := base.RoundTrip(req)
	if err != nil {
		return nil, fmt.Errorf("firefly: round trip: %w", err)
	}

	return resp, nil
}
