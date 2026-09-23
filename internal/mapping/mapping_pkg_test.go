package mapping_test

import (
	"testing"

	"github.com/stretchr/testify/suite"
)

// TestMapping is the single entry point for package mapping's test suites.
func TestMapping(t *testing.T) {
	suite.Run(t, new(AutoSuite))
	suite.Run(t, new(StatusSuite))
}
