package bank_test

import (
	"testing"

	"github.com/stretchr/testify/suite"
)

// TestBank is the single entry point for package bank's test suites.
func TestBank(t *testing.T) {
	suite.Run(t, new(ErrorsSuite))
	suite.Run(t, new(StatusSuite))
}
