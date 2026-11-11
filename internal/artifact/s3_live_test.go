package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/hayfever/mayfly/internal/config"
)

// TestS3CompatibleStore runs against a real S3-compatible server when
// MAYFLY_S3_TEST_ENDPOINT is set. The bucket must not exist or must be empty.
func TestS3CompatibleStore(t *testing.T) {
	endpoint := os.Getenv("MAYFLY_S3_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MAYFLY_S3_TEST_ENDPOINT is not set")
	}
	access := os.Getenv("MAYFLY_S3_TEST_ACCESS_KEY")
	secret := os.Getenv("MAYFLY_S3_TEST_SECRET_KEY")
	bucket := os.Getenv("MAYFLY_S3_TEST_BUCKET")
	if access == "" || secret == "" || bucket == "" {
		t.Fatal("MAYFLY_S3_TEST_ACCESS_KEY, MAYFLY_S3_TEST_SECRET_KEY, and MAYFLY_S3_TEST_BUCKET are required")
	}
	region := "us-east-1"
	ctx := context.Background()

	adminCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(access, secret, "")),
	)
	if err != nil {
		t.Fatal(err)
	}
	admin := s3.NewFromConfig(adminCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})
	if _, err := admin.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Logf("create bucket: %v", err)
	}

	t.Setenv("MAYFLY_S3_TEST_ACCESS", access)
	t.Setenv("MAYFLY_S3_TEST_SECRET", secret)
	store, err := NewS3(ctx, config.S3StoreConfig{
		Endpoint:     endpoint,
		Region:       region,
		Bucket:       bucket,
		AccessKeyEnv: "MAYFLY_S3_TEST_ACCESS",
		SecretKeyEnv: "MAYFLY_S3_TEST_SECRET",
		Prefix:       "mayfly-test",
		PathStyle:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("live s3 bundle")
	sum := sha256.Sum256(payload)
	digest := hex.EncodeToString(sum[:])
	key := "sessions/live/bundle.tar.gz"
	uri, err := store.Put(ctx, key, bytes.NewReader(payload), int64(len(payload)), digest, "application/gzip")
	if err != nil {
		t.Fatal(err)
	}
	if uri == "" {
		t.Fatal("empty URI")
	}
	rc, size, err := store.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if size != int64(len(payload)) || string(got) != string(payload) {
		t.Fatalf("round trip mismatch: size=%d got=%q", size, got)
	}
}
