package telegram

import (
	"strconv"
	"strings"
	"unicode/utf16"
)

const (
	// partLimit is the most UTF-16 code units one message may hold: safely below Telegram's
	// 4096, which it counts in UTF-16 code units, not bytes or runes (research R9).
	partLimit = 4000
	// lineSeparator joins a part's lines; a part never ends in one.
	lineSeparator = "\n"
)

// split turns a digest's lines into message texts. Lines that fit in partLimit go out as one
// message. Otherwise they are packed greedily at line boundaries: part 1 starts with the header
// line unchanged, and part k of n (k >= 2) with the header plus " (k/n)", a suffix that counts
// toward the limit. Every part carries at least one line after its header, so a single line too
// long for any message still goes out (and the Bot API rejects it visibly) rather than looping.
func split(lines []string) []string {
	whole := strings.Join(lines, lineSeparator)
	if units(whole) <= partLimit {
		return []string{whole}
	}

	// The suffix's width depends on the total, which is only known after packing. Packing with a
	// larger total never yields fewer parts, so raising the guess to the count converges; the
	// bound only guards against a bug turning that into an endless loop.
	total := 2
	parts := pack(lines, total)

	for range lines {
		if len(parts) == total {
			break
		}

		total = len(parts)
		parts = pack(lines, total)
	}

	return parts
}

// pack fills parts greedily, labelling continuation headers as parts of total.
func pack(lines []string, total int) []string {
	header := lines[0]
	separator := units(lineSeparator)

	parts := make([]string, 0, total)

	var current strings.Builder

	current.WriteString(header)
	size, bodyLines := units(header), 0

	for _, line := range lines[1:] {
		width := separator + units(line)

		if size+width > partLimit && bodyLines > 0 {
			parts = append(parts, current.String())

			next := continuationHeader(header, len(parts)+1, total)

			current.Reset()
			current.WriteString(next)
			size, bodyLines = units(next), 0
		}

		current.WriteString(lineSeparator)
		current.WriteString(line)

		size += width
		bodyLines++
	}

	return append(parts, current.String())
}

// continuationHeader is the header line of part k of total, for k >= 2.
func continuationHeader(header string, k, total int) string {
	return header + " (" + strconv.Itoa(k) + "/" + strconv.Itoa(total) + ")"
}

// units counts s the way Telegram measures a message, in UTF-16 code units, so an emoji outside
// the Basic Multilingual Plane counts 2.
func units(s string) int {
	n := 0

	for _, r := range s {
		// RuneLen is -1 only for a rune UTF-16 cannot encode; ranging over a string already turns
		// invalid bytes into U+FFFD, and JSON encoding would too, so such a rune costs 1.
		n += max(utf16.RuneLen(r), 1)
	}

	return n
}
