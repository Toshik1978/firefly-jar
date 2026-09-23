package telegram_test

import (
	"testing"

	"github.com/stretchr/testify/suite"
)

// TestTelegram is the single entry point for package telegram's test suites.
func TestTelegram(t *testing.T) {
	suite.Run(t, new(TelegramSuite))
}
