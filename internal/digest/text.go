package digest

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxTextRunes is the longest a description or unchecked detail may be, in runes, ellipsis
// included, so no single line can outgrow a Telegram message (contracts/digest.md).
const maxTextRunes = 60

// ellipsis marks text cut to maxTextRunes.
const ellipsis = "…"

// Strip removes every character that could break a line or reorder how one displays: control
// characters (Cc, tabs and line breaks included), format characters (Cf, which includes every bidi
// override and isolate) and the line and paragraph separators. Bank- and Firefly-supplied text
// passes through it before it is interpolated anywhere, in the digest and in the accounts table, so
// a counterparty or account name can neither add a line nor spoof one.
func Strip(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp) {
			return -1
		}

		return r
	}, s)
}

// text is the pipeline for long free text (descriptions, unchecked details), in an order the tests
// pin: Strip first, so a hidden character cannot shield an IBAN from the mask; then mask, so the
// length is measured on what is actually shown; then truncate.
func (rn *renderer) text(s string) string {
	return truncate(rn.redactor.Scrub(Strip(s)))
}

// truncate cuts s to maxTextRunes runes, the last of them the ellipsis, when it is longer.
func truncate(s string) string {
	if utf8.RuneCountInString(s) <= maxTextRunes {
		return s
	}

	runes := []rune(s)

	return string(runes[:maxTextRunes-1]) + ellipsis
}
