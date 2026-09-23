package email

import (
	"testing"

	"github.com/stretchr/testify/suite"
)

// TestEmail is the single entry point for package email's test suites.
func TestEmail(t *testing.T) {
	suite.Run(t, new(EmailSuite))
}
