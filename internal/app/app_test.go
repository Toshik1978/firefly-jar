package app_test

import (
	"testing"

	"github.com/stretchr/testify/suite"
)

// TestApp is the single entry point for package app's test suites.
func TestApp(t *testing.T) {
	suite.Run(t, new(CheckSuite))
	suite.Run(t, new(CLISuite))
	suite.Run(t, new(AcceptanceUS1Suite))
}
