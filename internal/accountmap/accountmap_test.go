package accountmap_test

import (
	"testing"

	"github.com/stretchr/testify/suite"
)

// TestAccountmap is the single entry point for package accountmap's test suites.
func TestAccountmap(t *testing.T) {
	suite.Run(t, new(AutoSuite))
	suite.Run(t, new(StatusSuite))
	suite.Run(t, new(OverrideSuite))
}
