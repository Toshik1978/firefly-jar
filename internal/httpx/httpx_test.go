package httpx_test

import (
	"testing"

	"github.com/stretchr/testify/suite"
)

// TestHTTPX is the single entry point for package httpx's test suites.
func TestHTTPX(t *testing.T) {
	suite.Run(t, new(RetrySuite))
	suite.Run(t, new(ClientSuite))
}
