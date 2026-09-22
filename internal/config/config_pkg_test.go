package config_test

import (
	"testing"

	"github.com/stretchr/testify/suite"
)

// TestConfig is the single entry point for package config's test suites.
func TestConfig(t *testing.T) {
	suite.Run(t, new(LoadSuite))
	suite.Run(t, new(ValidateSuite))
}
