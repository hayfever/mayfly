package gateway

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// TokenVersion prefixes every call token. A verifier rejects any other
// version rather than guessing at an older format.
const TokenVersion = "v1"

// ErrToken reports a call token that does not authorize the request.
var ErrToken = errors.New("invalid call token")

// Claims is the signed body of a call token. Tool is the remote tool name, or
// "*" for a discovery token that authorizes initialize and tools/list.
type Claims struct {
	SessionID  string `json:"sid"`
	Plugin     string `json:"plugin"`
	Tool       string `json:"tool"`
	Key        string `json:"key,omitempty"`
	ArgsSHA256 string `json:"args_sha256,omitempty"`
	Expires    int64  `json:"exp"`
}

// TokenScope is what one request asks a token to authorize. Empty fields are
// not checked, so a caller can verify a discovery token with only the session
// and plugin it expects.
type TokenScope struct {
	SessionID  string
	Plugin     string
	Tool       string
	ArgsSHA256 string
}

// SignToken returns v1.<claims>.<hmac>. The signature covers the version and
// the encoded claims, and the key never leaves the control plane or the shim.
func SignToken(key []byte, c Claims) (string, error) {
	if len(key) == 0 {
		return "", fmt.Errorf("%w: signing key is empty", ErrToken)
	}
	if c.SessionID == "" || c.Plugin == "" || c.Tool == "" {
		return "", fmt.Errorf("%w: claims need sid, plugin, and tool", ErrToken)
	}
	if c.Expires <= 0 {
		return "", fmt.Errorf("%w: claims need an expiry", ErrToken)
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("%w: encode claims: %v", ErrToken, err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	return TokenVersion + "." + encoded + "." + sign(key, TokenVersion+"."+encoded), nil
}

// VerifyToken checks the signature, the expiry, and the scope the request
// asks for. A discovery token (tool "*") authorizes any tool name.
func VerifyToken(key []byte, token string, want TokenScope, now time.Time) (Claims, error) {
	var zero Claims
	if len(key) == 0 {
		return zero, fmt.Errorf("%w: signing key is empty", ErrToken)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != TokenVersion {
		return zero, fmt.Errorf("%w: malformed token", ErrToken)
	}
	if !hmac.Equal([]byte(sign(key, TokenVersion+"."+parts[1])), []byte(parts[2])) {
		return zero, fmt.Errorf("%w: bad signature", ErrToken)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return zero, fmt.Errorf("%w: bad claims encoding", ErrToken)
	}
	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return zero, fmt.Errorf("%w: bad claims", ErrToken)
	}
	if c.Expires <= 0 || now.Unix() >= c.Expires {
		return zero, fmt.Errorf("%w: expired", ErrToken)
	}
	if want.SessionID != "" && c.SessionID != want.SessionID {
		return zero, fmt.Errorf("%w: token is for another session", ErrToken)
	}
	if want.Plugin != "" && c.Plugin != want.Plugin {
		return zero, fmt.Errorf("%w: token is for another plugin", ErrToken)
	}
	if want.Tool != "" && c.Tool != "*" && c.Tool != want.Tool {
		return zero, fmt.Errorf("%w: token is for another tool", ErrToken)
	}
	if want.ArgsSHA256 != "" && c.ArgsSHA256 != want.ArgsSHA256 {
		return zero, fmt.Errorf("%w: token is for other arguments", ErrToken)
	}
	return c, nil
}

// sign returns the base64url HMAC of one token segment.
func sign(key []byte, body string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(body))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
