package broker

import (
	"net/url"
	"strings"
	"testing"
)

// FuzzRedactURL checks the log-safe form of a configured URL. For arbitrary
// input the redaction must not panic, must keep no user info, query, or
// fragment, and must not change under a second call. The seeds cover the
// credential shapes a provider URL can take.
func FuzzRedactURL(f *testing.F) {
	for _, seed := range []string{
		"https://user:pass@example.com/v1?api_key=secret#frag",
		"http://example.com/v1",
		"https://example.com/v1/chat/completions?key=x",
		"",
		"   ",
		"not a url",
		"://",
		"http://",
		"http://[::1]:8080/v1",
		"https://user@host/path",
		"http://host?x=1",
		"https://user%3Apass@example.com/",
		"https://example.com/" + strings.Repeat("a", 4096) + "?key=secret",
		unparsableURL,
		"//user:pass@//",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		got := redactURL(raw)
		u, err := url.Parse(got)
		if err != nil {
			if got != unparsableURL {
				t.Fatalf("redactURL(%q) = %q, which does not parse", raw, got)
			}
		} else if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			t.Fatalf("redactURL(%q) = %q keeps user info, query, or fragment", raw, got)
		}
		if got == unparsableURL && strings.ContainsAny(got, "@?#") {
			t.Fatalf("placeholder %q looks like it carries credentials", got)
		}
		if again := redactURL(got); again != got {
			t.Fatalf("redactURL is not idempotent: %q -> %q -> %q", raw, got, again)
		}
	})
}
