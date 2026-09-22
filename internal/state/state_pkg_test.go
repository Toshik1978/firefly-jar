package state_test

import (
	"testing"

	"github.com/stretchr/testify/suite"
)

// TestState is the single entry point for package state's test suites.
func TestState(t *testing.T) {
	suite.Run(t, new(StateSuite))
}
