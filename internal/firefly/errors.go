package firefly

import (
	"errors"
	"fmt"
	"net/http"
)

// Sentinel error kinds the Firefly III client reports, wrapped with operational context. errors.Is
// recognizes each one through any depth of fmt.Errorf wrapping, so the run can tell "the token is
// wrong" (fix the config) from "this account is gone" (fix the mapping) from "the data was cut
// short" (the account is unchecked, never silently truncated).
var (
	// ErrUnauthorized means Firefly III rejected the personal access token (HTTP 401).
	ErrUnauthorized = errors.New("firefly: unauthorized")
	// ErrNotFound means the requested Firefly III resource does not exist (HTTP 404).
	ErrNotFound = errors.New("firefly: not found")
	// ErrDataIncomplete means the client stopped before reading every page, so the result would
	// be a truncation rather than the whole list (research R8's page cap).
	ErrDataIncomplete = errors.New("firefly: data incomplete")
)

// statusError maps a non-2xx status to an error that names only the status. The response body is
// deliberately never read into the error: it may echo the request (and so the query or token) or
// be an arbitrary proxy page, and a non-JSON body is reported by status code only (research R8).
func statusError(status int) error {
	switch status {
	case http.StatusUnauthorized:
		return fmt.Errorf("%w: Firefly token rejected (status %d)", ErrUnauthorized, status)
	case http.StatusNotFound:
		return fmt.Errorf("%w (status %d)", ErrNotFound, status)
	default:
		return fmt.Errorf("firefly: unexpected status %d", status)
	}
}
