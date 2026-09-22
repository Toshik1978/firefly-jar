// Package redact scrubs secrets and bank identifiers out of strings before they reach a log,
// error or digest.
package redact

import (
	"regexp"
	"slices"
	"strings"
)

// redactedPlaceholder replaces every secret and Telegram bot token Scrub finds.
const redactedPlaceholder = "[REDACTED]"

// maskedShort is what MaskIBAN returns for a string too short to show a first-four/last-four
// masked form without exposing most of it.
const maskedShort = "****"

// visibleEdgeLength is how many leading and trailing characters MaskIBAN and MaskHash leave
// visible around the masked middle.
const visibleEdgeLength = 4

// ibanPatternSource matches a standalone IBAN-shaped token in free text: two letters, two digits,
// then 11-30 more letters or digits (ISO 13616), case-insensitively (adapters normalize IBANs to
// upper case, but this also scrubs one pasted or logged in lower case). The \b anchors keep it
// from matching mid-token, such as the IBAN-shaped tail of a longer alphanumeric identifier.
const ibanPatternSource = `(?i)\b[A-Z]{2}\d{2}[A-Z0-9]{11,30}\b`

// telegramBotPatternSource matches the bot-token segment of a Telegram Bot API URL
// (https://api.telegram.org/bot<token>/...), so the token is scrubbed even when the caller never
// registered it as a secret.
const telegramBotPatternSource = `/bot[^/]+/`

// Redactor scrubs a fixed set of configured secrets, plus IBAN- and Telegram-bot-token-shaped
// substrings, out of arbitrary text.
type Redactor struct {
	// secrets holds the non-empty values passed to New, longest first, so a secret that
	// contains a shorter one is fully redacted before the shorter one's replacement would
	// otherwise leave a partial match behind.
	secrets         []string
	ibanPattern     *regexp.Regexp
	telegramPattern *regexp.Regexp
}

// New builds a Redactor for secrets, compiling its regular expressions once so Scrub never
// recompiles them. Empty strings among secrets are ignored, so a caller can pass optional config
// values straight through without filtering them first.
func New(secrets ...string) *Redactor {
	kept := make([]string, 0, len(secrets))

	for _, secret := range secrets {
		if secret != "" {
			kept = append(kept, secret)
		}
	}

	slices.SortFunc(kept, func(a, b string) int {
		return len(b) - len(a)
	})

	return &Redactor{
		secrets:         kept,
		ibanPattern:     regexp.MustCompile(ibanPatternSource),
		telegramPattern: regexp.MustCompile(telegramBotPatternSource),
	}
}

// Scrub returns s with every configured secret, every Telegram bot token and every IBAN-shaped
// substring replaced by a redaction marker.
func (r *Redactor) Scrub(s string) string {
	out := s

	for _, secret := range r.secrets {
		out = strings.ReplaceAll(out, secret, redactedPlaceholder)
	}

	out = r.telegramPattern.ReplaceAllString(out, "/bot"+redactedPlaceholder+"/")
	out = r.ibanPattern.ReplaceAllStringFunc(out, MaskIBAN)

	return out
}

// MaskIBAN masks s to its first four and last four characters, joined by an ellipsis. Strings of
// visibleEdgeLength*2 characters or fewer come back as maskedShort, since showing both edges of a
// string that short would show almost all of it.
func MaskIBAN(s string) string {
	if len(s) <= visibleEdgeLength*2 {
		return maskedShort
	}

	return s[:visibleEdgeLength] + "…" + s[len(s)-visibleEdgeLength:]
}

// MaskHash masks h to a "hash:" prefix followed by its first four and last four characters,
// joined by an ellipsis. Strings of visibleEdgeLength*2 characters or fewer come back as
// "hash:" + maskedShort, for the same reason as MaskIBAN.
func MaskHash(h string) string {
	if len(h) <= visibleEdgeLength*2 {
		return "hash:" + maskedShort
	}

	return "hash:" + h[:visibleEdgeLength] + "…" + h[len(h)-visibleEdgeLength:]
}
