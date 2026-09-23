package digest_test

import (
	"testing"

	"github.com/stretchr/testify/suite"
)

// TestDigest is the single entry point for package digest's test suites.
func TestDigest(t *testing.T) {
	suite.Run(t, new(RenderSuite))
}
