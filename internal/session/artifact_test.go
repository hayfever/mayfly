package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/hayfever/mayfly/internal/model"
	"github.com/hayfever/mayfly/internal/sessioncfg"
)

const testDigest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestResolveArtifactsAcceptsGrants(t *testing.T) {
	m := testManager(t)
	got, err := m.resolveArtifacts([]ArtifactRef{{
		Key:         "runs/competitor-watch/run.json",
		SHA256:      testDigest,
		Bytes:       1024,
		ContentType: " application/json ",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("artifacts = %d, want 1", len(got))
	}
	if got[0].ContentType != "application/json" {
		t.Fatalf("content type = %q", got[0].ContentType)
	}
}

func TestResolveArtifactsRejectsBadGrants(t *testing.T) {
	valid := ArtifactRef{Key: "runs/a.json", SHA256: testDigest, Bytes: 10, ContentType: "application/json"}
	many := make([]ArtifactRef, MaxSessionArtifacts+1)
	for i := range many {
		many[i] = ArtifactRef{
			Key:         "runs/" + string(rune('a'+i)) + ".json",
			SHA256:      testDigest,
			Bytes:       10,
			ContentType: "application/json",
		}
	}
	m := testManager(t)
	cases := []struct {
		name string
		refs []ArtifactRef
		want string
	}{
		{"key traversal", []ArtifactRef{{Key: "runs/../a.json", SHA256: testDigest, Bytes: 10, ContentType: "application/json"}}, "invalid artifact key"},
		{"absolute key", []ArtifactRef{{Key: "/etc/passwd", SHA256: testDigest, Bytes: 10, ContentType: "application/json"}}, "invalid artifact key"},
		{"digest shape", []ArtifactRef{{Key: "runs/a.json", SHA256: strings.ToUpper(testDigest), Bytes: 10, ContentType: "application/json"}}, "sha256"},
		{"zero bytes", []ArtifactRef{{Key: "runs/a.json", SHA256: testDigest, ContentType: "application/json"}}, "bytes"},
		{"too big", []ArtifactRef{{Key: "runs/a.json", SHA256: testDigest, Bytes: 1 << 26, ContentType: "application/json"}}, "max_artifact_bytes"},
		{"type shape", []ArtifactRef{{Key: "runs/a.json", SHA256: testDigest, Bytes: 10, ContentType: "json"}}, "invalid content type"},
		{"too many", many, "at most"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := m.resolveArtifacts(tc.refs)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}

	dup := []ArtifactRef{valid, valid}
	if _, err := m.resolveArtifacts(dup); err == nil || !strings.Contains(err.Error(), "repeated") {
		t.Fatalf("duplicate err = %v", err)
	}
}

func TestReadArtifactServesGrantedWindow(t *testing.T) {
	m := testManager(t)
	const key = "runs/a.json"
	putArtifact(t, m, key, "abcdefghij")
	m.sessions["s1"] = &instance{
		sess:      model.Session{ID: "s1"},
		tokenHash: hashToken("tok"),
		artifacts: map[string]sessioncfg.Artifact{
			key: {Key: key, SHA256: digestOf("abcdefghij"), Bytes: 10, ContentType: "application/json"},
		},
	}
	m.tokenIndex[hashToken("tok")] = "s1"

	window, err := m.ReadArtifact(context.Background(), "s1", "tok", key, 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	if string(window.Content) != "cde" || window.Offset != 2 || window.Bytes != 3 || window.Total != 10 {
		t.Fatalf("window = %+v", window)
	}
	if window.NextOffset == nil || *window.NextOffset != 5 {
		t.Fatalf("next = %v", window.NextOffset)
	}

	tail, err := m.ReadArtifact(context.Background(), "s1", "tok", key, 8, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if string(tail.Content) != "ij" || tail.NextOffset != nil {
		t.Fatalf("tail = %+v", tail)
	}

	end, err := m.ReadArtifact(context.Background(), "s1", "tok", key, 10, 24<<10)
	if err != nil {
		t.Fatal(err)
	}
	if end.Bytes != 0 || end.NextOffset != nil || end.Total != 10 {
		t.Fatalf("end = %+v", end)
	}
}

func TestReadArtifactRejectsOtherTokens(t *testing.T) {
	m := testManager(t)
	const key = "runs/a.json"
	putArtifact(t, m, key, "abc")
	m.sessions["s1"] = &instance{
		sess:      model.Session{ID: "s1"},
		tokenHash: hashToken("tok"),
		artifacts: map[string]sessioncfg.Artifact{
			key: {Key: key, SHA256: testDigest, Bytes: 3, ContentType: "text/plain"},
		},
	}
	m.tokenIndex[hashToken("tok")] = "s1"

	if _, err := m.ReadArtifact(context.Background(), "s1", "other", key, 0, 10); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("foreign token err = %v", err)
	}
	if _, err := m.ReadArtifact(context.Background(), "s2", "tok", key, 0, 10); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("foreign session err = %v", err)
	}
	if _, err := m.ReadArtifact(context.Background(), "s1", "tok", "runs/b.json", 0, 10); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ungranted key err = %v", err)
	}
}

func digestOf(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func TestReadArtifactWindowKeepsRunesWhole(t *testing.T) {
	m := testManager(t)
	const key = "runs/u.json"
	const content = "ab日本"
	putArtifact(t, m, key, content)
	m.sessions["s1"] = &instance{
		sess:      model.Session{ID: "s1"},
		tokenHash: hashToken("tok"),
		artifacts: map[string]sessioncfg.Artifact{
			key: {Key: key, SHA256: digestOf(content), Bytes: int64(len(content)), ContentType: "text/plain"},
		},
	}
	m.tokenIndex[hashToken("tok")] = "s1"
	// Four bytes would end inside 日; the window stops before it.
	window, err := m.ReadArtifact(context.Background(), "s1", "tok", key, 0, 4)
	if err != nil {
		t.Fatal(err)
	}
	if string(window.Content) != "ab" || window.NextOffset == nil || *window.NextOffset != 2 {
		t.Fatalf("window = %q next %v", window.Content, window.NextOffset)
	}
	rest, err := m.ReadArtifact(context.Background(), "s1", "tok", key, 2, 1<<10)
	if err != nil {
		t.Fatal(err)
	}
	if string(rest.Content) != "日本" {
		t.Fatalf("rest = %q", rest.Content)
	}
}

func TestReadArtifactRejectsDigestMismatch(t *testing.T) {
	m := testManager(t)
	const key = "runs/a.json"
	putArtifact(t, m, key, "abcdefghij")
	m.sessions["s1"] = &instance{
		sess:      model.Session{ID: "s1"},
		tokenHash: hashToken("tok"),
		artifacts: map[string]sessioncfg.Artifact{
			// Same size, different content: the size check alone passes.
			key: {Key: key, SHA256: digestOf("ABCDEFGHIJ"), Bytes: 10, ContentType: "application/json"},
		},
	}
	m.tokenIndex[hashToken("tok")] = "s1"
	if _, err := m.ReadArtifact(context.Background(), "s1", "tok", key, 0, 4); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("err = %v, want sha256 mismatch", err)
	}
}

func putArtifact(t *testing.T, m *Manager, key, content string) {
	t.Helper()
	if _, err := m.store.Put(context.Background(), key, strings.NewReader(content), int64(len(content)), "", "text/plain"); err != nil {
		t.Fatal(err)
	}
}
