package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hayfever/mayfly/internal/config"
)

func TestLocalStoreRoundTrip(t *testing.T) {
	store, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("hello artifact")
	sum := sha256.Sum256(payload)
	digest := hex.EncodeToString(sum[:])
	uri, err := store.Put(context.Background(), "sessions/s1/bundle.tar.gz", strings.NewReader(string(payload)), int64(len(payload)), digest, "application/gzip")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(uri, "file://") {
		t.Fatalf("unexpected URI %q", uri)
	}
	rc, size, err := store.Get(context.Background(), "sessions/s1/bundle.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len(payload)) || string(got) != string(payload) {
		t.Fatalf("round trip mismatch: size=%d got=%q", size, got)
	}
	if _, _, err := store.Get(context.Background(), "missing"); err != ErrNotFound {
		t.Fatalf("missing key error = %v", err)
	}
}

func TestLocalStoreRejectsTraversal(t *testing.T) {
	s, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	local, ok := s.(*localStore)
	if !ok {
		t.Fatal("unexpected store type")
	}
	name, err := local.resolve("../../etc/passwd")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(name, local.dir+"/") {
		t.Fatalf("traversal escaped the store: %s", name)
	}
}

func TestS3ObjectKey(t *testing.T) {
	store := &s3Store{cfg: config.S3StoreConfig{Bucket: "bkt", Prefix: "mayfly"}}
	if got := store.objectKey("sessions/a/b.tar.gz"); got != "mayfly/sessions/a/b.tar.gz" {
		t.Fatalf("key = %q", got)
	}
	empty := &s3Store{cfg: config.S3StoreConfig{Bucket: "bkt"}}
	if got := empty.objectKey("sessions/a/b.tar.gz"); got != "sessions/a/b.tar.gz" {
		t.Fatalf("key without prefix = %q", got)
	}
}

func TestArtifactKey(t *testing.T) {
	key := Key("abc", time.Unix(1700000000, 0))
	if key != "sessions/abc/1700000000-abc.tar.gz" {
		t.Fatalf("key = %q", key)
	}
}

func TestS3StorePutGet(t *testing.T) {
	var putPath, putAuth string
	var checksumHeaders []string
	var stored []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			putPath = r.URL.Path
			putAuth = r.Header.Get("Authorization")
			for name := range r.Header {
				lower := strings.ToLower(name)
				if strings.HasPrefix(lower, "x-amz-checksum") || lower == "x-amz-sdk-checksum-algorithm" {
					checksumHeaders = append(checksumHeaders, lower)
				}
			}
			data, _ := io.ReadAll(r.Body)
			stored = data
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			if r.URL.Path != putPath {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(stored)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()
	t.Setenv("TEST_S3_ACCESS", "access")
	t.Setenv("TEST_S3_SECRET", "secret")
	store, err := NewS3(context.Background(), config.S3StoreConfig{
		Endpoint:     server.URL,
		Region:       "us-east-1",
		Bucket:       "bkt",
		AccessKeyEnv: "TEST_S3_ACCESS",
		SecretKeyEnv: "TEST_S3_SECRET",
		Prefix:       "prod",
		PathStyle:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("bundle bytes")
	sum := sha256.Sum256(payload)
	digest := hex.EncodeToString(sum[:])
	uri, err := store.Put(context.Background(), "sessions/s1/b.tar.gz", bytes.NewReader(payload), int64(len(payload)), digest, "application/gzip")
	if err != nil {
		t.Fatal(err)
	}
	if uri != "s3://bkt/prod/sessions/s1/b.tar.gz" {
		t.Fatalf("uri = %q", uri)
	}
	if putPath != "/bkt/prod/sessions/s1/b.tar.gz" {
		t.Fatalf("put path = %q", putPath)
	}
	if !strings.HasPrefix(putAuth, "AWS4-HMAC-SHA256 ") {
		t.Fatalf("authorization = %q", putAuth)
	}
	// S3-compatible stores such as R2, B2, and older MinIO reject the SDK's
	// default checksum headers.
	if len(checksumHeaders) > 0 {
		t.Fatalf("unexpected checksum headers: %v", checksumHeaders)
	}
	rc, size, err := store.Get(context.Background(), "sessions/s1/b.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if size != int64(len(payload)) || string(got) != string(payload) {
		t.Fatalf("get size=%d got=%q", size, got)
	}
}
