package bank

import "errors"

// Sentinel error kinds a Provider or Authorizer reports, normally wrapped in an *Error for
// operational context. errors.Is recognizes each one through any depth of fmt.Errorf wrapping
// (T023).
var (
	// ErrConsentExpired means the session's ValidUntil has passed.
	ErrConsentExpired = errors.New("consent expired")
	// ErrConsentRevoked means the provider reported the consent as revoked or closed.
	ErrConsentRevoked = errors.New("consent revoked")
	// ErrRateLimited means the provider throttled the request.
	ErrRateLimited = errors.New("rate limited")
	// ErrDataIncomplete means the provider returned a transaction missing a field this tool
	// requires.
	ErrDataIncomplete = errors.New("data incomplete")
	// ErrStateMismatch means the redirect Authorizer.Complete received does not carry the State
	// value its Pending issued.
	ErrStateMismatch = errors.New("state mismatch")
)

// Error pairs a sentinel Kind with operational Detail, so a caller can errors.Is against Kind while
// logs and errors still carry context. Detail is operational text only: never a transaction
// description or counterparty name (constitution §V).
type Error struct {
	Kind   error
	Detail string
}

// Error implements the error interface.
func (e *Error) Error() string {
	return e.Kind.Error() + ": " + e.Detail
}

// Unwrap returns Kind, so errors.Is and errors.As see through Error to its sentinel.
func (e *Error) Unwrap() error {
	return e.Kind
}
