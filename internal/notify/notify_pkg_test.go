package notify_test

import (
	"testing"

	"github.com/stretchr/testify/suite"
)

// TestNotify is the single entry point for package notify's test suites.
func TestNotify(t *testing.T) {
	suite.Run(t, new(FanOutSuite))
}
