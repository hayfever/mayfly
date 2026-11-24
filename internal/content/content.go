// Package content defines the structured parts one tool result can carry:
// text and image data. The same part type travels through the MCP client, the
// tool gateway's ledger, the session conversation, and the model request.
// Limits keep one result and one session bounded, and Text renders parts for
// logs without exposing image bytes.
package content

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/hayfever/mayfly/internal/textutil"
)

// Part types.
const (
	TypeText  = "text"
	TypeImage = "image"
)

// Default limits. One image and the images of one result are 5 MiB, which
// keeps the base64 form under the 8 MiB default journal step bound after
// JSON overhead. One session holds 8 MiB, which keeps a model request of
// base64 data under the 16 MiB default broker body bound.
const (
	DefaultMaxImageBytes        = 5 << 20
	DefaultMaxResultImageBytes  = 5 << 20
	DefaultMaxSessionImageBytes = 8 << 20
)

// Limits bound image content at each scope.
type Limits struct {
	// MaxImageBytes bounds one image part.
	MaxImageBytes int64
	// MaxResultImageBytes bounds the images of one tool result.
	MaxResultImageBytes int64
	// MaxSessionImageBytes bounds the images one session may deliver to the
	// model and store in its conversation.
	MaxSessionImageBytes int64
}

// WithDefaults fills every zero or negative limit with its default. A caller
// can never disable a bound by leaving the field unset.
func (l Limits) WithDefaults() Limits {
	if l.MaxImageBytes <= 0 {
		l.MaxImageBytes = DefaultMaxImageBytes
	}
	if l.MaxResultImageBytes <= 0 {
		l.MaxResultImageBytes = DefaultMaxResultImageBytes
	}
	if l.MaxSessionImageBytes <= 0 {
		l.MaxSessionImageBytes = DefaultMaxSessionImageBytes
	}
	return l
}

// Part is one piece of tool output. Text parts carry Text. Image parts carry
// an allowed MIMEType and the decoded image in Data; JSON encodes Data as
// base64. A part never carries both.
type Part struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	MIMEType string `json:"mime_type,omitempty"`
	Data     []byte `json:"data,omitempty"`
}

// TextPart returns a text part.
func TextPart(text string) Part { return Part{Type: TypeText, Text: text} }

// ImagePart returns an image part.
func ImagePart(mime string, data []byte) Part {
	return Part{Type: TypeImage, MIMEType: mime, Data: data}
}

// IsImage reports whether p is an image part.
func (p Part) IsImage() bool { return p.Type == TypeImage }

// Size returns the number of image bytes p holds.
func (p Part) Size() int64 {
	if !p.IsImage() {
		return 0
	}
	return int64(len(p.Data))
}

// allowedMIMETypes is the set a tool result image may carry. The gateway and
// the runtime accept nothing else, so a provider never receives a type it
// cannot read.
var allowedMIMETypes = []string{
	"image/png",
	"image/jpeg",
	"image/gif",
	"image/webp",
}

// NormalizeMIMEType returns the lower-case type without parameters and
// reports whether it is allowed.
func NormalizeMIMEType(mime string) (string, bool) {
	value := strings.ToLower(strings.TrimSpace(mime))
	if idx := strings.IndexByte(value, ';'); idx >= 0 {
		value = strings.TrimSpace(value[:idx])
	}
	for _, allowed := range allowedMIMETypes {
		if value == allowed {
			return value, true
		}
	}
	return value, false
}

// AllowedMIMETypes returns the allowed types in sorted order.
func AllowedMIMETypes() []string {
	out := append([]string(nil), allowedMIMETypes...)
	sort.Strings(out)
	return out
}

// ImageDigest returns the hex SHA-256 of p's bytes.
func ImageDigest(p Part) string {
	sum := sha256.Sum256(p.Data)
	return hex.EncodeToString(sum[:])
}

// ImageSummary renders p without its bytes, for a log, event, or text-only
// provider. It is the only image rendering that may leave the process.
func ImageSummary(p Part) string {
	return fmt.Sprintf("[image %s, %d bytes, sha256:%s]", p.MIMEType, len(p.Data), ImageDigest(p))
}

// ImagePlaceholder renders p as text that says why the model did not receive
// the image.
func ImagePlaceholder(p Part, reason string) string {
	return fmt.Sprintf("[image %s, %d bytes, sha256:%s not sent: %s]",
		p.MIMEType, len(p.Data), ImageDigest(p), reason)
}

// Text renders parts as newline-joined text. Text parts keep their text;
// image parts become their summary. The result never contains image bytes.
func Text(parts []Part) string {
	lines := make([]string, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case TypeImage:
			lines = append(lines, ImageSummary(p))
		default:
			lines = append(lines, p.Text)
		}
	}
	return strings.Join(lines, "\n")
}

// HasImage reports whether any part is an image.
func HasImage(parts []Part) bool {
	for _, p := range parts {
		if p.IsImage() {
			return true
		}
	}
	return false
}

// ImageCount returns the number of image parts.
func ImageCount(parts []Part) int {
	count := 0
	for _, p := range parts {
		if p.IsImage() {
			count++
		}
	}
	return count
}

// ImageBytes returns the total image bytes.
func ImageBytes(parts []Part) int64 {
	var total int64
	for _, p := range parts {
		total += p.Size()
	}
	return total
}

// WithoutImages replaces every image part with a text placeholder that names
// the type, size, digest, and reason. The bytes are dropped.
func WithoutImages(parts []Part, reason string) []Part {
	out := make([]Part, 0, len(parts))
	for _, p := range parts {
		if p.IsImage() {
			out = append(out, TextPart(ImagePlaceholder(p, reason)))
			continue
		}
		out = append(out, p)
	}
	return out
}

// ValidateImage reports why p cannot be delivered, or the empty string when
// it can. It checks the MIME type, the per-image limit, and that data exists.
func ValidateImage(p Part, limits Limits) string {
	limits = limits.WithDefaults()
	mime, ok := NormalizeMIMEType(p.MIMEType)
	if mime == "" {
		return "mimeType is missing"
	}
	if !ok {
		return fmt.Sprintf("%q is not an allowed MIME type (allowed: %s)", p.MIMEType, strings.Join(AllowedMIMETypes(), ", "))
	}
	if len(p.Data) == 0 {
		return "image data is empty"
	}
	if int64(len(p.Data)) > limits.MaxImageBytes {
		return fmt.Sprintf("%s is %d bytes, over the %d-byte per-image limit", mime, len(p.Data), limits.MaxImageBytes)
	}
	return ""
}

// Sanitize validates every image part against limits. An invalid part becomes
// a text error and reports rejected. It returns the accepted image bytes,
// which the caller charges to its session budget.
func Sanitize(parts []Part, limits Limits) (clean []Part, imageBytes int64, rejected bool) {
	limits = limits.WithDefaults()
	clean = make([]Part, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case TypeImage:
			if reason := ValidateImage(p, limits); reason != "" {
				clean = append(clean, TextPart("ERROR: tool result image was rejected: "+reason))
				rejected = true
				continue
			}
			if imageBytes+int64(len(p.Data)) > limits.MaxResultImageBytes {
				clean = append(clean, TextPart(fmt.Sprintf(
					"ERROR: tool result image was rejected: images total %d bytes, over the %d-byte per-result limit",
					imageBytes+int64(len(p.Data)), limits.MaxResultImageBytes)))
				rejected = true
				continue
			}
			imageBytes += int64(len(p.Data))
			clean = append(clean, p)
		default:
			clean = append(clean, p)
		}
	}
	return clean, imageBytes, rejected
}

// TruncateText caps the total bytes of text parts. Image parts pass through
// unchanged: a text cap must never slice image data. A cut adds one note.
func TruncateText(parts []Part, max int) []Part {
	if max < 0 {
		max = 0
	}
	out := make([]Part, 0, len(parts))
	left := max
	cut := false
	for _, p := range parts {
		if p.Type != TypeText {
			out = append(out, p)
			continue
		}
		if left <= 0 {
			cut = true
			continue
		}
		if len(p.Text) > left {
			p.Text = textutil.Truncate(p.Text, left)
			if p.Text == "" {
				cut = true
				continue
			}
			cut = true
		}
		left -= len(p.Text)
		out = append(out, p)
	}
	if cut {
		out = append(out, TextPart(fmt.Sprintf("\n(truncated at %d bytes)", max)))
	}
	return out
}

// Budget bounds a byte total. It is safe for concurrent use.
type Budget struct {
	mu   sync.Mutex
	max  int64
	used int64
}

// NewBudget returns a budget starting at used bytes.
func NewBudget(max, used int64) *Budget {
	if max <= 0 {
		max = DefaultMaxSessionImageBytes
	}
	if used < 0 {
		used = 0
	}
	return &Budget{max: max, used: used}
}

// Accept charges n bytes when they fit and reports whether it did.
func (b *Budget) Accept(n int64) bool {
	if n < 0 {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used+n > b.max {
		return false
	}
	b.used += n
	return true
}

// Used returns the charged bytes.
func (b *Budget) Used() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}

// Max returns the budget's bound.
func (b *Budget) Max() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.max
}
