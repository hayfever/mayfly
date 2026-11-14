// Package textutil holds small string helpers shared across packages.
package textutil

import "unicode/utf8"

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
