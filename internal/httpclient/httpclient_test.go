package httpclient_test

import (
	"testing"

	"github.com/stretchr/testify/suite"
)

// TestHTTPClient is the single entry point for package httpclient's test suites.
func TestHTTPClient(t *testing.T) {
	suite.Run(t, new(RetrySuite))
	suite.Run(t, new(ClientSuite))
}
