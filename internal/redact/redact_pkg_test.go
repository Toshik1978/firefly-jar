package redact_test

import (
	"testing"

	"github.com/stretchr/testify/suite"
)

// TestRedact is the single entry point for package redact's test suites.
func TestRedact(t *testing.T) {
	suite.Run(t, new(RedactorSuite))
}
