package bank_test

import (
	"github.com/stretchr/testify/suite"

	"github.com/Toshik1978/firefly-jar/internal/bank"
)

// StatusSuite covers bank.Status.String(), which the digest and logs use to render a bank
// transaction's settlement state (bank.go doc comment). It exercises every named value plus an
// out-of-range one, so the default branch is covered too.
type StatusSuite struct {
	suite.Suite
}

// TestStringRendersEachNamedValue asserts String renders the lowercase word the digest and logs
// expect for each settlement state.
func (s *StatusSuite) TestStringRendersEachNamedValue() {
	cases := []struct {
		name   string
		status bank.Status
		want   string
	}{
		{"booked", bank.Booked, "booked"},
		{"pending", bank.Pending, "pending"},
		{"void", bank.Void, "void"},
	}

	for _, tc := range cases {
		s.Run(tc.name, func() {
			s.Equal(tc.want, tc.status.String())
		})
	}
}

// TestStringOfUnknownValueRendersUnknown asserts an out-of-range Status (never produced by the
// mapper today, but reachable if a provider adapter adds a status the mapper does not yet
// recognize) renders "unknown" rather than panicking or printing a raw integer.
func (s *StatusSuite) TestStringOfUnknownValueRendersUnknown() {
	s.Equal("unknown", bank.Status(99).String())
}
