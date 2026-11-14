package platformclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReadArtifactSendsWindowRequest(t *testing.T) {
	var gotPath, gotQuery, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery, gotAuth = r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization")
		next := int64(5)
		_ = json.NewEncoder(w).Encode(ArtifactWindow{
			Key:         "runs/a.json",
			Offset:      2,
			Bytes:       3,
			Total:       10,
			NextOffset:  &next,
			ContentType: "application/json",
			Content:     "cde",
		})
	}))
	defer server.Close()

	client := New(server.URL, "s1", "tok")
	window, err := client.ReadArtifact(context.Background(), "runs/a.json", 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/internal/v1/sessions/s1/artifacts" {
		t.Fatalf("path = %q", gotPath)
	}
	if !strings.Contains(gotQuery, "key=runs%2Fa.json") || !strings.Contains(gotQuery, "offset=2") || !strings.Contains(gotQuery, "limit=3") {
		t.Fatalf("query = %q", gotQuery)
	}
	if gotAuth != "Bearer tok" {
		t.Fatalf("auth = %q", gotAuth)
	}
	if window.Content != "cde" || window.NextOffset == nil || *window.NextOffset != 5 {
		t.Fatalf("window = %+v", window)
	}
}

func TestReadArtifactReportsStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "artifact not found", http.StatusNotFound)
	}))
	defer server.Close()

	_, err := New(server.URL, "s1", "tok").ReadArtifact(context.Background(), "runs/a.json", 0, 10)
	if err == nil || !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "artifact not found") {
		t.Fatalf("err = %v", err)
	}
}
