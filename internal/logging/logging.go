// Package logging builds the process logger: a JSON handler that writes every record at the
// configured level to the log file, and a text handler that writes only WARN and above to
// stderr. Both handlers share one redacting ReplaceAttr, so a secret or bank identifier is masked
// identically wherever it would otherwise reach a log line (R14, FR-038, FR-039).
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/Toshik1978/firefly-jar/internal/redact"
)

// redactedPlaceholder is what New's ReplaceAttr substitutes for the whole value of a known secret
// key, regardless of shape.
const redactedPlaceholder = "[REDACTED]"

// sessionIDKey is the attribute key whose value New's ReplaceAttr truncates rather than scrubs.
const sessionIDKey = "session_id"

// sessionIDVisibleLength is how many leading characters of a session id are kept visible before
// the ellipsis. A value with this many characters or fewer would show almost, or all, of the id
// that way, so it is redacted outright instead (constitution §V: an identifier is masked, never
// printed in full, wherever it leaves the process).
const sessionIDVisibleLength = 4

// logFileMode is the permission OpenFile creates, or leaves unchanged on, the log file with:
// owner read/write only, since the file carries masked identifiers.
const logFileMode = 0o600

// New builds the process logger from a JSON file handler at level and a text stderr handler
// fixed at WARN and above, both routed through one ReplaceAttr built from r.
func New(file, stderr io.Writer, level slog.Level, r *redact.Redactor) *slog.Logger {
	replaceAttr := newReplaceAttr(r)

	fileHandler := slog.NewJSONHandler(file, &slog.HandlerOptions{
		Level:       level,
		ReplaceAttr: replaceAttr,
	})
	stderrHandler := slog.NewTextHandler(stderr, &slog.HandlerOptions{
		Level:       slog.LevelWarn,
		ReplaceAttr: replaceAttr,
	})

	return slog.New(slog.NewMultiHandler(fileHandler, stderrHandler))
}

// OpenFile opens path for appending log records, creating it with logFileMode if it does not
// exist yet. Reopening an existing file never truncates it, since every run is a new process that
// must keep what earlier runs logged. A pre-existing file with wider permissions (created before
// this feature existed, or under a looser umask) is tightened to logFileMode, since the file
// carries masked identifiers and must never be group- or world-readable.
func OpenFile(path string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Clean(path), os.O_APPEND|os.O_CREATE|os.O_WRONLY, logFileMode)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}

	if err := f.Chmod(logFileMode); err != nil {
		_ = f.Close()

		return nil, fmt.Errorf("tighten log file permissions: %w", err)
	}

	return f, nil
}

// newReplaceAttr builds the slog.HandlerOptions.ReplaceAttr shared by both of New's handlers: a
// known secret key is redacted outright, session_id is redacted outright once it is too short to
// mask without showing most of it, a string value passes through r.Scrub, and any other
// non-numeric, non-bool, non-time value (an error, a fmt.Stringer, or anything else the KindAny
// bucket holds) is rendered to text and scrubbed the same way, so a secret or an IBAN embedded in
// a wrapped error's message is masked identically to one passed as a plain string attribute.
func newReplaceAttr(r *redact.Redactor) func(groups []string, a slog.Attr) slog.Attr {
	return func(_ []string, a slog.Attr) slog.Attr {
		switch {
		case isSecretKey(a.Key):
			a.Value = slog.StringValue(redactedPlaceholder)
		case a.Key == sessionIDKey:
			a.Value = slog.StringValue(truncateSessionID(a.Value.String()))
		case a.Value.Kind() == slog.KindString:
			a.Value = slog.StringValue(r.Scrub(a.Value.String()))
		case a.Value.Kind() == slog.KindAny:
			a.Value = slog.StringValue(r.Scrub(stringifyAny(a.Value.Any())))
		}

		return a
	}
}

// isSecretKey reports whether key names an attribute whose entire value is a credential by
// definition, so ReplaceAttr must redact it outright instead of scrubbing only recognized shapes
// within it.
func isSecretKey(key string) bool {
	switch key {
	case "token", "password", "private_key", "authorization":
		return true
	default:
		return false
	}
}

// truncateSessionID renders id to its first sessionIDVisibleLength characters plus an ellipsis.
// An id of sessionIDVisibleLength characters or fewer is redacted outright instead: masking would
// show almost, or all, of it, and slicing it would panic on an out-of-range bound.
func truncateSessionID(id string) string {
	if len(id) <= sessionIDVisibleLength {
		return redactedPlaceholder
	}

	return id[:sessionIDVisibleLength] + "…"
}

// stringifyAny renders a KindAny attribute value to text so newReplaceAttr can scrub it like any
// other string. It always goes through fmt.Sprint rather than calling Error() or String()
// directly: fmt already prefers those methods (so a wrapped error renders the same either way),
// but it also detects a nil receiver before calling one and recovers from any panic the method
// itself raises, printing a placeholder such as "<nil>" instead of crashing the whole run. A
// helper that returns a typed-nil error by mistake, or an Error()/String() method that
// dereferences its receiver, must never take the process down through a log call.
func stringifyAny(v any) string {
	return fmt.Sprint(v)
}
