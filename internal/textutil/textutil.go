// Package textutil holds small string helpers shared across packages.
package textutil

import (
	"strings"
	"unicode/utf8"
)

// Replacement is the rune substituted for each byte that is not part of a
// valid UTF-8 sequence. A webhook header can carry arbitrary bytes, and every
// string derived from it must be valid UTF-8 before it reaches JSON or a
// session record.
const Replacement = "\uFFFD"

// Valid returns s with each invalid UTF-8 byte replaced by Replacement. It
// returns s unchanged when it is already valid.
func Valid(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return strings.ToValidUTF8(s, Replacement)
}

// Truncate returns at most limit bytes of s, backing up to the start of a
// UTF-8 sequence so the result stays valid text.
func Truncate(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
