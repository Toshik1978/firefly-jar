package reconcile_test

import (
	"testing"

	"github.com/stretchr/testify/suite"
)

// TestReconcile is the single entry point for package reconcile's test suites.
func TestReconcile(t *testing.T) {
	suite.Run(t, new(ReconcileSuite))
}
