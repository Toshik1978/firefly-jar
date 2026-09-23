package report_test

import (
	"testing"

	"github.com/stretchr/testify/suite"
)

// TestReport is the single entry point for package report's test suites.
func TestReport(t *testing.T) {
	suite.Run(t, new(ReportSuite))
}
