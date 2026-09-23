package logging_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/suite"

	"github.com/Toshik1978/firefly-jar/internal/logging"
	"github.com/Toshik1978/firefly-jar/internal/redact"
)

// TestLogging is the single entry point for package logging's test suites.
func TestLogging(t *testing.T) {
	suite.Run(t, new(LoggingSuite))
}

// LoggingSuite covers logging.New and logging.OpenFile: the file handler writes JSON at the
// configured level, stderr only ever sees WARN and above as text, and a shared ReplaceAttr
// redacts secrets and identifiers on both handlers (FR-038, FR-039, R14).
type LoggingSuite struct {
	suite.Suite

	fileBuf   *bytes.Buffer
	stderrBuf *bytes.Buffer
}

func (s *LoggingSuite) SetupTest() {
	s.fileBuf = new(bytes.Buffer)
	s.stderrBuf = new(bytes.Buffer)
}

// fileLines returns the non-empty lines written to the file buffer, one per log record.
func (s *LoggingSuite) fileLines() []string {
	trimmed := strings.TrimSpace(s.fileBuf.String())
	if trimmed == "" {
		return nil
	}

	return strings.Split(trimmed, "\n")
}

// decodeFileRecord requires exactly one JSON record was written to the file buffer and decodes it.
func (s *LoggingSuite) decodeFileRecord() map[string]any {
	lines := s.fileLines()
	s.Require().Len(lines, 1, "expected exactly one file log record")

	var record map[string]any
	s.Require().NoError(json.Unmarshal([]byte(lines[0]), &record))

	return record
}

func (s *LoggingSuite) TestInfoRecordGoesToFileWriterOnlyAsJSON() {
	logger := logging.New(s.fileBuf, s.stderrBuf, slog.LevelInfo, redact.New())

	logger.Info("account sync completed", "accounts_checked", 3)

	record := s.decodeFileRecord()
	s.Equal("INFO", record[slog.LevelKey])
	s.Equal("account sync completed", record[slog.MessageKey])
	s.EqualValues(3, record["accounts_checked"])
	s.Empty(s.stderrBuf.String(), "an INFO record must never reach stderr")
}

func (s *LoggingSuite) TestWarnRecordGoesToBothAndStderrUsesTextFormat() {
	logger := logging.New(s.fileBuf, s.stderrBuf, slog.LevelInfo, redact.New())

	logger.Warn("digest delivery failed")

	record := s.decodeFileRecord()
	s.Equal("WARN", record[slog.LevelKey])
	s.Equal("digest delivery failed", record[slog.MessageKey])

	stderrText := s.stderrBuf.String()
	s.Contains(stderrText, "level=WARN")
	s.Contains(stderrText, `msg="digest delivery failed"`)
}

func (s *LoggingSuite) TestWarnLevelFileHandlerDropsInfo() {
	logger := logging.New(s.fileBuf, s.stderrBuf, slog.LevelWarn, redact.New())

	logger.Info("account sync completed")

	s.Empty(s.fileBuf.String(), "a log_level: warn file handler must drop INFO records")
	s.Empty(s.stderrBuf.String())

	logger.Warn("digest delivery failed")

	s.Len(s.fileLines(), 1, "the same logger must still record WARN once the level allows it")
	s.Contains(s.stderrBuf.String(), "level=WARN")
}

func (s *LoggingSuite) TestReplaceAttrMasksIBANShapedStringAttributes() {
	logger := logging.New(s.fileBuf, s.stderrBuf, slog.LevelInfo, redact.New())

	logger.Info("bank fetch completed", "account", "LT121000011101001000")

	record := s.decodeFileRecord()
	s.Equal("LT12…1000", record["account"])
	s.NotContains(s.fileBuf.String(), "LT121000011101001000")
}

func (s *LoggingSuite) TestReplaceAttrRedactsKnownSecretKeys() {
	cases := []struct {
		name string
		key  string
	}{
		{name: "token", key: "token"},
		{name: "password", key: "password"},
		{name: "private_key", key: "private_key"},
		{name: "authorization", key: "authorization"},
	}

	for _, tc := range cases {
		s.Run(tc.name, func() {
			s.SetupTest()
			logger := logging.New(s.fileBuf, s.stderrBuf, slog.LevelInfo, redact.New())

			logger.Warn("credential attached to record", tc.key, "raw-secret-value-not-iban-shaped")

			record := s.decodeFileRecord()
			s.Equal("[REDACTED]", record[tc.key])
			s.NotContains(s.fileBuf.String(), "raw-secret-value-not-iban-shaped")

			stderrText := s.stderrBuf.String()
			s.Contains(stderrText, tc.key+"=[REDACTED]")
			s.NotContains(stderrText, "raw-secret-value-not-iban-shaped")
		})
	}
}

func (s *LoggingSuite) TestReplaceAttrTruncatesSessionID() {
	logger := logging.New(s.fileBuf, s.stderrBuf, slog.LevelInfo, redact.New())

	logger.Info("session opened", "session_id", "abcdef123456")

	record := s.decodeFileRecord()
	s.Equal("abcd…", record["session_id"])
	s.NotContains(s.fileBuf.String(), "abcdef123456")
}

// TestReplaceAttrRedactsShortSessionIDOutright pins the behavior for a session id shorter than
// (or equal to) the visible prefix ReplaceAttr normally keeps: rather than panicking on an
// out-of-range slice bound, or printing the id in full (constitution §V — an identifier must never
// leave the process unmasked), it is redacted outright, the same as a known secret key.
func (s *LoggingSuite) TestReplaceAttrRedactsShortSessionIDOutright() {
	cases := map[string]string{
		"empty string":     "",
		"one character":    "a",
		"three characters": "abc",
		"exactly four":     "abcd",
	}

	for name, id := range cases {
		s.Run(name, func() {
			s.SetupTest()
			logger := logging.New(s.fileBuf, s.stderrBuf, slog.LevelInfo, redact.New())

			logger.Info("session opened", "session_id", id)

			record := s.decodeFileRecord()
			s.Equal("[REDACTED]", record["session_id"])
			if id != "" {
				s.NotContains(s.fileBuf.String(), id)
			}
		})
	}
}

// TestReplaceAttrScrubsErrorAttrValues covers a KindAny attribute holding an error: New's
// ReplaceAttr must render it to text (err.Error()) and scrub that text the same way it scrubs an
// ordinary string attribute, so a secret or an IBAN embedded in a wrapped error's message never
// reaches the file or stderr unmasked.
func (s *LoggingSuite) TestReplaceAttrScrubsErrorAttrValues() {
	r := redact.New("s3cr3t-token-value")
	logger := logging.New(s.fileBuf, s.stderrBuf, slog.LevelWarn, r)

	wrapped := fmt.Errorf(
		"bank fetch failed for account LT121000011101001000 using s3cr3t-token-value: %w",
		errors.New("timeout"),
	)
	logger.Warn("request failed", "err", wrapped)

	record := s.decodeFileRecord()
	errText, ok := record["err"].(string)
	s.Require().True(ok, "expected the err attr to be rendered as a string")
	s.Equal("bank fetch failed for account LT12…1000 using [REDACTED]: timeout", errText)
	s.NotContains(s.fileBuf.String(), "LT121000011101001000")
	s.NotContains(s.fileBuf.String(), "s3cr3t-token-value")

	stderrText := s.stderrBuf.String()
	s.Contains(stderrText, `err="bank fetch failed for account LT12…1000 using [REDACTED]: timeout"`)
	s.NotContains(stderrText, "LT121000011101001000")
	s.NotContains(stderrText, "s3cr3t-token-value")
}

// derefError is an error whose Error method dereferences its receiver, so calling it directly on
// a typed-nil *derefError panics with a nil pointer dereference, the way a real "operation failed:
// %w"-style error occasionally does when a helper returns a typed nil by mistake.
type derefError struct {
	msg string
}

func (e *derefError) Error() string {
	return "deref failed: " + e.msg
}

// TestReplaceAttrRendersTypedNilErrorWithoutPanicking covers a KindAny attribute holding a
// typed-nil pointer that implements error: New's ReplaceAttr must never call Error() directly,
// since that panics on a nil receiver that dereferences itself. It must render through a path that
// recovers from that panic and produces a sane placeholder instead of crashing the whole run.
func (s *LoggingSuite) TestReplaceAttrRendersTypedNilErrorWithoutPanicking() {
	logger := logging.New(s.fileBuf, s.stderrBuf, slog.LevelWarn, redact.New())

	var nilErr *derefError

	var record map[string]any
	s.NotPanics(func() {
		logger.Warn("request failed", "err", error(nilErr))
		record = s.decodeFileRecord()
	})

	s.Equal("<nil>", record["err"])
	s.Contains(s.stderrBuf.String(), "err=<nil>")
}

func (s *LoggingSuite) TestReplaceAttrRedactsAttrsAddedViaWith() {
	logger := logging.New(s.fileBuf, s.stderrBuf, slog.LevelInfo, redact.New()).With(
		"token", "raw-secret-value-not-iban-shaped",
		"account", "LT121000011101001000",
	)

	logger.Info("session opened")

	record := s.decodeFileRecord()
	s.Equal("[REDACTED]", record["token"])
	s.Equal("LT12…1000", record["account"])
	s.NotContains(s.fileBuf.String(), "raw-secret-value-not-iban-shaped")
	s.NotContains(s.fileBuf.String(), "LT121000011101001000")
}

func (s *LoggingSuite) TestReplaceAttrRedactsAttrsInsideGroup() {
	logger := logging.New(s.fileBuf, s.stderrBuf, slog.LevelInfo, redact.New())

	logger.Info("bank fetch completed", slog.Group("bank",
		"account", "LT121000011101001000",
		"token", "raw-secret-value-not-iban-shaped",
	))

	record := s.decodeFileRecord()
	group, ok := record["bank"].(map[string]any)
	s.Require().True(ok, "expected a bank group in the record")
	s.Equal("LT12…1000", group["account"])
	s.Equal("[REDACTED]", group["token"])
	s.NotContains(s.fileBuf.String(), "raw-secret-value-not-iban-shaped")
	s.NotContains(s.fileBuf.String(), "LT121000011101001000")
}

func (s *LoggingSuite) TestReplaceAttrScrubsConfiguredSecretInAttributeValue() {
	r := redact.New("s3cr3t-token-value")
	logger := logging.New(s.fileBuf, s.stderrBuf, slog.LevelInfo, r)

	logger.Warn("request failed", "detail", "auth using s3cr3t-token-value failed")

	record := s.decodeFileRecord()
	s.Equal("auth using [REDACTED] failed", record["detail"])
	s.NotContains(s.fileBuf.String(), "s3cr3t-token-value")
	s.NotContains(s.stderrBuf.String(), "s3cr3t-token-value")
}

func (s *LoggingSuite) TestOpenFileCreatesWithMode0600AndAppendsOnReopen() {
	path := filepath.Join(s.T().TempDir(), "firefly-jar.log")

	first, err := logging.OpenFile(path)
	s.Require().NoError(err)

	_, err = first.WriteString("first line\n")
	s.Require().NoError(err)

	info, err := os.Stat(path)
	s.Require().NoError(err)
	s.Equal(fs.FileMode(0o600), info.Mode().Perm())

	s.Require().NoError(first.Close())

	second, err := logging.OpenFile(path)
	s.Require().NoError(err)

	_, err = second.WriteString("second line\n")
	s.Require().NoError(err)
	s.Require().NoError(second.Close())

	data, err := os.ReadFile(filepath.Clean(path))
	s.Require().NoError(err)
	s.Equal("first line\nsecond line\n", string(data), "reopening must append, never truncate")
}

// TestOpenFileTightensPreExistingWiderPermissions covers a log file that already exists with
// permissions wider than 0600 (e.g. created by a different umask before this feature existed):
// OpenFile must tighten it to 0600 rather than leaving the wider mode in place, since the file may
// carry masked identifiers.
func (s *LoggingSuite) TestOpenFileTightensPreExistingWiderPermissions() {
	path := filepath.Join(s.T().TempDir(), "firefly-jar.log")
	s.Require().NoError(os.WriteFile(path, []byte("pre-existing line\n"), 0o644))

	f, err := logging.OpenFile(path)
	s.Require().NoError(err)
	s.Require().NoError(f.Close())

	info, err := os.Stat(path)
	s.Require().NoError(err)
	s.Equal(fs.FileMode(0o600), info.Mode().Perm(), "OpenFile must tighten a pre-existing wider mode")
}
