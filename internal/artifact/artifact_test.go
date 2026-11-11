package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/hayden/mayfly/internal/config"
)

func TestSignV4KnownVector(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", "bytes=0-9")
	empty := sha256.Sum256(nil)
	signV4(req, hex.EncodeToString(empty[:]), "AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "", "us-east-1", time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC))
	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("signature mismatch\n got: %s\nwant: %s", got, want)
	}
}

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

func TestS3ObjectURL(t *testing.T) {
	store := &s3Store{
		cfg:      config.S3StoreConfig{Bucket: "bkt", Prefix: "mayfly", PathStyle: true},
		endpoint: mustURL(t, "https://s3.example.com"),
	}
	u := store.objectURL("sessions/a/b.tar.gz")
	if u.Path != "/bkt/mayfly/sessions/a/b.tar.gz" {
		t.Fatalf("path = %q", u.Path)
	}
	store.cfg.PathStyle = false
	store.endpoint = mustURL(t, "https://s3.example.com")
	u = store.objectURL("sessions/a/b.tar.gz")
	if u.Host != "bkt.s3.example.com" || u.Path != "/mayfly/sessions/a/b.tar.gz" {
		t.Fatalf("virtual host URL = %s", u.String())
	}
}

func TestArtifactKey(t *testing.T) {
	key := Key("abc", time.Unix(1700000000, 0))
	if key != "sessions/abc/1700000000-abc.tar.gz" {
		t.Fatalf("key = %q", key)
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestS3StorePutGet(t *testing.T) {
	var putPath, putAuth string
	var stored []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			putPath = r.URL.Path
			putAuth = r.Header.Get("Authorization")
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
	store, err := NewS3(config.S3StoreConfig{
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
