package logging_test

import (
	"testing"

	"github.com/stretchr/testify/suite"
)

// TestLogging is the single entry point for package logging's test suites.
func TestLogging(t *testing.T) {
	suite.Run(t, new(LoggingSuite))
}
