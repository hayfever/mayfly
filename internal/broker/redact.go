package broker

import (
	"errors"
	"net/url"
	"strings"
)

// unparsableURL stands in for a configured URL that net/url rejects. The raw
// string is never returned, because it may still carry credentials.
const unparsableURL = "<unparsable url>"

// redactPasses bounds the re-rendering loop in redactURL. URL rendering
// settles in one or two passes; more than that means an input the helper
// cannot make stable, which gets the placeholder.
const redactPasses = 4

// redactURL drops the parts of raw that can carry credentials: user info,
// query, and fragment. A log line still names the upstream by scheme, host, and
// path. The result is stable under a second call.
func redactURL(raw string) string {
	if raw == unparsableURL {
		// The placeholder is itself a valid URL path, so keep it as it is
		// instead of escaping it again.
		return unparsableURL
	}
	// Rendering is not always a fixed point: "//@//" parses to an empty
	// authority and renders as "//", which renders again as "". Strip until
	// the text stops changing so a second call returns the same string.
	out := raw
	for range redactPasses {
		u, err := url.Parse(out)
		if err != nil {
			return unparsableURL
		}
		u.User = nil
		u.RawQuery = ""
		u.Fragment = ""
		next := u.String()
		if next == out {
			return out
		}
		out = next
	}
	return unparsableURL
}

// redactedError carries the text of a wrapped error with its URL redacted. The
// original error stays reachable through Unwrap.
type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }

func (e *redactedError) Unwrap() error { return e.err }

// redactError prepares err for a log line. The text of a *url.Error, direct or
// wrapped, embeds the full request URL, which may hold user info or a token in
// the query. Such an error is copied with a redacted URL; a wrapped one is
// rebuilt with the same text apart from that URL. Other errors pass through.
func redactError(err error) error {
	if err == nil {
		return nil
	}
	var uerr *url.Error
	if !errors.As(err, &uerr) || uerr.URL == "" {
		return err
	}
	// The copy keeps *url.Error's Timeout method and its Err chain, so only
	// the URL changes in the logged text.
	redacted := *uerr
	redacted.URL = redactURL(uerr.URL)
	if err == error(uerr) { //nolint:errorlint // err is the *url.Error itself, not a wrapper, so the copy keeps its type.
		return &redacted
	}
	// Replacing the rendered inner error keeps the wrapper's own wording
	// without matching the URL against the escaped form in the text.
	msg := strings.ReplaceAll(err.Error(), uerr.Error(), redacted.Error())
	return &redactedError{msg: msg, err: err}
}
