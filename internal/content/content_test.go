package content

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestLimitsDefaultsNeverUnlimited(t *testing.T) {
	limits := Limits{}.WithDefaults()
	if limits.MaxImageBytes != DefaultMaxImageBytes ||
		limits.MaxResultImageBytes != DefaultMaxResultImageBytes ||
		limits.MaxSessionImageBytes != DefaultMaxSessionImageBytes {
		t.Fatalf("defaults = %+v", limits)
	}
	limits = Limits{MaxImageBytes: -1, MaxResultImageBytes: -2, MaxSessionImageBytes: -3}.WithDefaults()
	if limits.MaxImageBytes <= 0 || limits.MaxResultImageBytes <= 0 || limits.MaxSessionImageBytes <= 0 {
		t.Fatalf("negative limits did not fall back: %+v", limits)
	}
}

func TestNormalizeMIMEType(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"image/png", "image/png", true},
		{"IMAGE/JPEG", "image/jpeg", true},
		{" image/webp; charset=binary ", "image/webp", true},
		{"image/gif", "image/gif", true},
		{"image/svg+xml", "image/svg+xml", false},
		{"text/plain", "text/plain", false},
		{"", "", false},
	}
	for _, tc := range cases {
		got, ok := NormalizeMIMEType(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("NormalizeMIMEType(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestSanitizeEnforcesMIMEAndLimits(t *testing.T) {
	limits := Limits{MaxImageBytes: 8, MaxResultImageBytes: 12, MaxSessionImageBytes: 12}
	ok := ImagePart("image/png", []byte("12345678"))
	tooBig := ImagePart("image/png", []byte("123456789"))
	badMime := ImagePart("image/svg+xml", []byte("abc"))
	empty := ImagePart("image/png", nil)

	parts, bytes, rejected := Sanitize([]Part{TextPart("before"), ok}, limits)
	if rejected || bytes != 8 || len(parts) != 2 || !parts[1].IsImage() {
		t.Fatalf("valid parts = %+v bytes=%d rejected=%v", parts, bytes, rejected)
	}

	for name, part := range map[string]Part{"too big": tooBig, "bad mime": badMime, "empty": empty} {
		parts, _, rejected := Sanitize([]Part{part}, limits)
		if !rejected || len(parts) != 1 || parts[0].IsImage() || !strings.HasPrefix(parts[0].Text, "ERROR: tool result image was rejected:") {
			t.Errorf("%s: parts = %+v rejected=%v", name, parts, rejected)
		}
	}

	parts, _, rejected = Sanitize([]Part{ok, ok}, limits)
	if !rejected || len(parts) != 2 || !parts[0].IsImage() || parts[1].IsImage() {
		t.Fatalf("per-result limit: parts = %+v rejected=%v", parts, rejected)
	}
	if !strings.Contains(parts[1].Text, "per-result limit") {
		t.Fatalf("per-result error = %q", parts[1].Text)
	}
}

func TestTextNeverContainsImageBytes(t *testing.T) {
	data := []byte("this looks like a payload")
	parts := []Part{TextPart("caption"), ImagePart("image/png", data)}
	text := Text(parts)
	if !strings.Contains(text, "caption") || !strings.Contains(text, "[image image/png, 25 bytes, sha256:") {
		t.Fatalf("text = %q", text)
	}
	if strings.Contains(text, base64.StdEncoding.EncodeToString(data)) {
		t.Fatalf("text contains image bytes: %q", text)
	}
}

func TestWithoutImagesKeepsBytesOut(t *testing.T) {
	parts := []Part{TextPart("caption"), ImagePart("image/png", []byte("abc"))}
	out := WithoutImages(parts, "model is text only")
	if HasImage(out) {
		t.Fatal("image survived WithoutImages")
	}
	if !strings.Contains(Text(out), "not sent: model is text only") {
		t.Fatalf("placeholder = %q", Text(out))
	}
}

func TestTruncateTextLeavesImagesWhole(t *testing.T) {
	data := []byte("0123456789")
	parts := []Part{TextPart(strings.Repeat("x", 100)), ImagePart("image/png", data)}
	out := TruncateText(parts, 10)
	if len(out) != 3 {
		t.Fatalf("parts = %+v", out)
	}
	if out[0].Text != strings.Repeat("x", 10) {
		t.Fatalf("text = %q", out[0].Text)
	}
	if !out[1].IsImage() || string(out[1].Data) != string(data) {
		t.Fatalf("image changed: %+v", out[1])
	}
	if !strings.Contains(out[2].Text, "(truncated at 10 bytes)") {
		t.Fatalf("note = %q", out[2].Text)
	}
}

func TestBudget(t *testing.T) {
	b := NewBudget(10, 4)
	if !b.Accept(6) || b.Used() != 10 {
		t.Fatalf("accept 6 = used %d", b.Used())
	}
	if b.Accept(1) {
		t.Fatal("budget accepted past its bound")
	}
	if b.Accept(-1) {
		t.Fatal("budget accepted a negative charge")
	}
}
