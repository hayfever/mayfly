package artifact

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"

	"github.com/hayfever/mayfly/internal/config"
)

// newS3Store builds an S3-compatible store. When access_key_env and
// secret_key_env are set, the store uses those static credentials. When both
// are empty, it uses the default AWS chain: environment, shared config, web
// identity, ECS, or instance metadata. That covers IAM roles for service
// accounts without static keys.
func newS3Store(ctx context.Context, cfg config.S3StoreConfig) (*s3Store, error) {
	options := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(cfg.Region)}
	if cfg.AccessKeyEnv != "" || cfg.SecretKeyEnv != "" {
		accessKey := os.Getenv(cfg.AccessKeyEnv)
		secretKey := os.Getenv(cfg.SecretKeyEnv)
		if accessKey == "" || secretKey == "" {
			return nil, fmt.Errorf("S3 credentials: environment variables %s and %s must be set", cfg.AccessKeyEnv, cfg.SecretKeyEnv)
		}
		var sessionToken string
		if cfg.SessionTokenEnv != "" {
			sessionToken = os.Getenv(cfg.SessionTokenEnv)
		}
		options = append(options, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(accessKey, secretKey, sessionToken),
		))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, options...)
	if err != nil {
		return nil, fmt.Errorf("S3 credentials: %w", err)
	}
	endpoint := ""
	if cfg.Endpoint != "" {
		endpoint, err = parseEndpoint(cfg.Endpoint)
		if err != nil {
			return nil, err
		}
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
		}
		o.UsePathStyle = cfg.PathStyle
		// The SDK defaults add CRC32 checksum headers and trailer checksums to
		// every upload. R2, B2, and older MinIO reject them, so send checksums
		// only when an operation requires one.
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})
	return &s3Store{
		cfg:    cfg,
		client: client,
	}, nil
}

// NewS3 exposes the S3 store constructor for tests and embedders.
func NewS3(ctx context.Context, cfg config.S3StoreConfig) (Store, error) {
	return newS3Store(ctx, cfg)
}

type s3Store struct {
	cfg    config.S3StoreConfig
	client *s3.Client
}

// parseEndpoint returns the configured endpoint as an absolute URL.
func parseEndpoint(raw string) (string, error) {
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		return "", fmt.Errorf("invalid s3 endpoint %q: scheme must be http or https", raw)
	}
	return strings.TrimRight(raw, "/"), nil
}

// objectKey joins the configured prefix and the artifact key.
func (s *s3Store) objectKey(key string) string {
	return strings.TrimPrefix(path.Join("/", s.cfg.Prefix, key), "/")
}

func (s *s3Store) Put(ctx context.Context, key string, r io.Reader, size int64, _ string, contentType string) (string, error) {
	if contentType == "" {
		contentType = "application/gzip"
	}
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.cfg.Bucket),
		Key:           aws.String(s.objectKey(key)),
		Body:          r,
		ContentLength: aws.Int64(size),
		ContentType:   aws.String(contentType),
	})
	if err != nil {
		return "", fmt.Errorf("s3 put: %w", err)
	}
	return "s3://" + s.cfg.Bucket + "/" + s.objectKey(key), nil
}

func (s *s3Store) Get(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(s.objectKey(key)),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, 0, ErrNotFound
		}
		return nil, 0, fmt.Errorf("s3 get: %w", err)
	}
	var size int64
	if out.ContentLength != nil {
		size = *out.ContentLength
	}
	return out.Body, size, nil
}

func isNotFound(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound", "404":
			return true
		}
	}
	return false
}
