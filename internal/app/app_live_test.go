//go:build live

package app_test

import (
	"testing"

	"github.com/stretchr/testify/suite"
)

// TestApp is the single entry point for package app's test suites under the live build tag: every
// suite app_test.go runs, plus LiveSuite. app_test.go carries the !live tag, so exactly one TestApp
// exists in either build. Keep the two lists in sync (.claude/CLAUDE.md, Testing).
func TestApp(t *testing.T) {
	suite.Run(t, new(CheckSuite))
	suite.Run(t, new(CLISuite))
	suite.Run(t, new(AcceptanceUS1Suite))
	suite.Run(t, new(AuthCommandSuite))
	suite.Run(t, new(ConsentSuite))
	suite.Run(t, new(AcceptanceUS2Suite))
	suite.Run(t, new(AccountsCommandSuite))
	suite.Run(t, new(AcceptanceUS3Suite))
	suite.Run(t, new(IsolationSuite))
	suite.Run(t, new(DeliverySuite))
	suite.Run(t, new(FailFastSuite))
	suite.Run(t, new(AcceptanceUS4Suite))
	suite.Run(t, new(PrivacySuite))
	suite.Run(t, new(ReadOnlyGuaranteeSuite))
	suite.Run(t, new(LiveSuite))
}
