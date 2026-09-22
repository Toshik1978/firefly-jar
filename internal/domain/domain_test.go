package domain_test

import (
	"testing"

	"github.com/stretchr/testify/suite"
)

// TestDomain is the single entry point for package domain's test suites.
func TestDomain(t *testing.T) {
	suite.Run(t, new(AmountSuite))
	suite.Run(t, new(WindowSuite))
}
