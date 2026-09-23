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

// spacedIBANPatternSource matches a candidate IBAN in its print form: the country code and check
// digits, then 4-character groups separated by single spaces, the last group possibly shorter
// (ISO 13616's paper format), case-insensitively. It deliberately over-matches, since a word after
// the IBAN has the same shape as a group; maskSpacedIBAN trims such words off and rejects a
// candidate that is mostly letters, so ordinary text is never masked.
const spacedIBANPatternSource = `(?i)\b[A-Z]{2}\d{2}(?: [A-Z0-9]{4}){2,7}(?: [A-Z0-9]{1,3})?\b`

// minBBANLength is the shortest basic bank account number any IBAN country uses (Norway, 11
// characters), so a spaced candidate with less after its check digits is not an IBAN.
const minBBANLength = 11

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
	secrets           []string
	ibanPattern       *regexp.Regexp
	spacedIBANPattern *regexp.Regexp
	telegramPattern   *regexp.Regexp
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
		secrets:           kept,
		ibanPattern:       regexp.MustCompile(ibanPatternSource),
		spacedIBANPattern: regexp.MustCompile(spacedIBANPatternSource),
		telegramPattern:   regexp.MustCompile(telegramBotPatternSource),
	}
}

// Scrub returns s with every configured secret, every Telegram bot token and every IBAN-shaped
// substring, compact or in 4-character groups, replaced by a redaction marker.
func (r *Redactor) Scrub(s string) string {
	out := s

	for _, secret := range r.secrets {
		out = strings.ReplaceAll(out, secret, redactedPlaceholder)
	}

	out = r.telegramPattern.ReplaceAllString(out, "/bot"+redactedPlaceholder+"/")
	out = r.spacedIBANPattern.ReplaceAllStringFunc(out, maskSpacedIBAN)
	out = r.ibanPattern.ReplaceAllStringFunc(out, MaskIBAN)

	return out
}

// maskSpacedIBAN masks a spacedIBANPattern match like MaskIBAN masks a compact IBAN, computed from
// the compact characters. Trailing groups without a digit are words that happened to follow the
// IBAN, so they are kept as text after the mask. What is left is masked only if its account part
// (after the check digits) is long enough for an IBAN and at least half digits, which every IBAN
// format satisfies and a run of words does not; otherwise the match comes back unchanged.
func maskSpacedIBAN(match string) string {
	groups := strings.Split(match, " ")

	kept := len(groups)
	for kept > 1 && !strings.ContainsFunc(groups[kept-1], isDigit) {
		kept--
	}

	bban := strings.Join(groups[1:kept], "")
	digits := len(bban) - len(strings.Map(dropDigit, bban))

	if len(bban) < minBBANLength || digits*2 < len(bban) {
		return match
	}

	masked := MaskIBAN(groups[0] + bban)
	if kept == len(groups) {
		return masked
	}

	return masked + " " + strings.Join(groups[kept:], " ")
}

// isDigit reports whether r is an ASCII digit, the only digits an IBAN contains.
func isDigit(r rune) bool {
	return r >= '0' && r <= '9'
}

// dropDigit is a strings.Map mapping that deletes ASCII digits and keeps everything else.
func dropDigit(r rune) rune {
	if isDigit(r) {
		return -1
	}

	return r
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
