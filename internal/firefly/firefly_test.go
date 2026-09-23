package firefly

import (
	"testing"

	"github.com/stretchr/testify/suite"
)

// TestFirefly is the single entry point for package firefly's test suites.
func TestFirefly(t *testing.T) {
	suite.Run(t, new(ReadOnlySuite))
	suite.Run(t, new(AccountsSuite))
	suite.Run(t, new(TransactionsSuite))
}
