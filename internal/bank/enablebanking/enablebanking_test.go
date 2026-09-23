package enablebanking_test

import (
	"testing"

	"github.com/stretchr/testify/suite"
)

// TestEnablebanking is the single entry point for package enablebanking's test suites.
func TestEnablebanking(t *testing.T) {
	suite.Run(t, new(SignerSuite))
	suite.Run(t, new(TransactionsSuite))
	suite.Run(t, new(AuthSuite))
}
