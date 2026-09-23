//go:build live

package app_test

import (
	"testing"

	"github.com/stretchr/testify/suite"
)

// TestAppLive is the entry point for the live-tagged suites of package app. It is kept out of
// app_test.go, whose TestApp is the package's single entry point under the default build: the
// `live` tag adds this second entry point only for a build most developers, and CI's `task check`,
// never compile.
func TestAppLive(t *testing.T) {
	suite.Run(t, new(LiveSuite))
}
