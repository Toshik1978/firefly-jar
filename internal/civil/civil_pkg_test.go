package civil_test

import (
	"testing"

	"github.com/stretchr/testify/suite"
)

// TestCivil is the single entry point for package civil's test suites.
func TestCivil(t *testing.T) {
	suite.Run(t, new(CivilSuite))
}
