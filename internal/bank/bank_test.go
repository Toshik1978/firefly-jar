package bank_test

import (
	"fmt"

	"github.com/stretchr/testify/suite"

	"github.com/Toshik1978/firefly-jar/internal/bank"
)

// ErrorsSuite covers internal/bank's sentinel errors and the Error wrapper (T023, data-model.md "Bank
// side", constitution §IV): errors.Is must work through fmt.Errorf wrapping for every sentinel, the
// sentinels must be distinct from one another, and *bank.Error must unwrap to its Kind while keeping
// its Detail both in the message and reachable through errors.As.
type ErrorsSuite struct {
	suite.Suite
}

// sentinelCases returns every sentinel the controller-fixed bank API exposes, named for table-driven
// subtests.
func (s *ErrorsSuite) sentinelCases() []struct {
	name string
	err  error
} {
	return []struct {
		name string
		err  error
	}{
		{"consent expired", bank.ErrConsentExpired},
		{"consent revoked", bank.ErrConsentRevoked},
		{"rate limited", bank.ErrRateLimited},
		{"data incomplete", bank.ErrDataIncomplete},
		{"state mismatch", bank.ErrStateMismatch},
	}
}

// TestSentinelIsThroughWrapping asserts errors.Is still recognizes each sentinel after it has been
// wrapped once with fmt.Errorf, the way a package boundary wraps it (wrapcheck).
func (s *ErrorsSuite) TestSentinelIsThroughWrapping() {
	for _, tc := range s.sentinelCases() {
		s.Run(tc.name, func() {
			wrapped := fmt.Errorf("call provider: %w", tc.err)

			s.Require().ErrorIs(wrapped, tc.err)
		})
	}
}

// TestSentinelIsThroughDoubleWrapping asserts errors.Is still recognizes each sentinel after two
// levels of fmt.Errorf wrapping, matching a sentinel crossing more than one package boundary.
func (s *ErrorsSuite) TestSentinelIsThroughDoubleWrapping() {
	for _, tc := range s.sentinelCases() {
		s.Run(tc.name, func() {
			wrapped := fmt.Errorf("adapter: %w", fmt.Errorf("call provider: %w", tc.err))

			s.Require().ErrorIs(wrapped, tc.err)
		})
	}
}

// TestSentinelsAreDistinct asserts no sentinel satisfies errors.Is for a different sentinel, so a
// caller switching on them with errors.Is never confuses one failure mode for another.
func (s *ErrorsSuite) TestSentinelsAreDistinct() {
	cases := s.sentinelCases()

	for i, a := range cases {
		for j, b := range cases {
			if i == j {
				continue
			}

			s.Run(a.name+" vs "+b.name, func() {
				s.NotErrorIs(a.err, b.err, "%s must not be errors.Is %s", a.name, b.name)
			})
		}
	}
}

// TestErrorSatisfiesIsDirectly asserts a *bank.Error built around a sentinel satisfies errors.Is for
// that sentinel without any wrapping in between.
func (s *ErrorsSuite) TestErrorSatisfiesIsDirectly() {
	err := &bank.Error{Kind: bank.ErrRateLimited, Detail: "retry-after: 30s"}

	s.Require().ErrorIs(err, bank.ErrRateLimited)
}

// TestErrorSatisfiesIsWhenWrapped asserts a *bank.Error still satisfies errors.Is for its Kind once
// wrapped by fmt.Errorf, the way a caller across a package boundary would receive it.
func (s *ErrorsSuite) TestErrorSatisfiesIsWhenWrapped() {
	err := &bank.Error{Kind: bank.ErrConsentExpired, Detail: "valid_until in the past"}
	wrapped := fmt.Errorf("check consent: %w", err)

	s.Require().ErrorIs(wrapped, bank.ErrConsentExpired)
}

// TestErrorUnwrapReturnsKind asserts Unwrap returns exactly the Kind sentinel, not a copy or a
// re-wrapped value.
func (s *ErrorsSuite) TestErrorUnwrapReturnsKind() {
	err := &bank.Error{Kind: bank.ErrDataIncomplete, Detail: "missing booking_date"}

	s.Require().Equal(bank.ErrDataIncomplete, err.Unwrap())
}

// TestErrorMessageIncludesDetail asserts Error() includes the Detail text, so a logged or wrapped
// message still carries the operational context.
func (s *ErrorsSuite) TestErrorMessageIncludesDetail() {
	err := &bank.Error{Kind: bank.ErrStateMismatch, Detail: "returned state does not match pending"}

	s.Contains(err.Error(), "returned state does not match pending")
}

// TestErrorAsExtractsDetail asserts errors.As can pull a *bank.Error back out of a wrapped chain and
// that its Detail survived the round trip.
func (s *ErrorsSuite) TestErrorAsExtractsDetail() {
	original := &bank.Error{Kind: bank.ErrConsentRevoked, Detail: "provider reported CLOSED_SESSION"}
	wrapped := fmt.Errorf("refresh session: %w", original)

	var extracted *bank.Error

	s.Require().ErrorAs(wrapped, &extracted)
	s.Equal("provider reported CLOSED_SESSION", extracted.Detail)
	s.Equal(bank.ErrConsentRevoked, extracted.Kind)
}
