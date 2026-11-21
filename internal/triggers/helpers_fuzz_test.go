package triggers

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"testing"
)

// fuzzHeader builds a header from name/value pairs.
func fuzzHeader(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

// fuzzFlip returns a copy of b with one byte changed. An empty input gains
// one byte, so the result always differs from the input.
func fuzzFlip(b []byte) []byte {
	out := append([]byte(nil), b...)
	if len(out) == 0 {
		return []byte{0}
	}
	out[len(out)/2] ^= 0x01
	return out
}

// fuzzFlipHex changes the first hex digit of s to a different digit. The
// result stays valid hex, so it covers a signature whose bytes differ and not
// only one that fails to decode.
func fuzzFlipHex(s string) string {
	for i, c := range []byte(s) {
		switch {
		case c >= '0' && c <= '9':
			return s[:i] + string('0'+(c-'0'+1)%10) + s[i+1:]
		case c >= 'a' && c <= 'f':
			return s[:i] + string('a'+(c-'a'+1)%6) + s[i+1:]
		case c >= 'A' && c <= 'F':
			return s[:i] + string('A'+(c-'A'+1)%6) + s[i+1:]
		}
	}
	return s + "0"
}

// fuzzSlackSignature signs body with the Slack v0 scheme for ts.
func fuzzSlackSignature(secret, ts string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = fmt.Fprintf(mac, "v0:%s:%s", ts, body)
	return "v0=" + hex.EncodeToString(mac.Sum(nil))
}

// fuzzCheckError fails unless err is nil, ErrUnauthorized, or ErrBadRequest.
// Parse promises no other error class for a webhook request.
func fuzzCheckError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	if !errors.Is(err, ErrUnauthorized) && !errors.Is(err, ErrBadRequest) {
		t.Fatalf("unexpected error class: %v", err)
	}
}

// fuzzAbs returns the absolute value of a small fuzz-derived offset.
func fuzzAbs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
