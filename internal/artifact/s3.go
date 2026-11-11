package artifact

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/hayfever/mayfly/internal/config"
)

// sha256Hex returns the lowercase hex SHA-256 of data.
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// newS3Store builds an S3-compatible store from configuration.
func newS3Store(cfg config.S3StoreConfig) (*s3Store, error) {
	accessKey := os.Getenv(cfg.AccessKeyEnv)
	secretKey := os.Getenv(cfg.SecretKeyEnv)
	if accessKey == "" || secretKey == "" {
		return nil, fmt.Errorf("S3 credentials: environment variables %s and %s must be set", cfg.AccessKeyEnv, cfg.SecretKeyEnv)
	}
	var token string
	if cfg.SessionTokenEnv != "" {
		token = os.Getenv(cfg.SessionTokenEnv)
	}
	endpoint, err := parseEndpoint(cfg.Endpoint, cfg.Region)
	if err != nil {
		return nil, err
	}
	return &s3Store{
		cfg:        cfg,
		endpoint:   endpoint,
		accessKey:  accessKey,
		secretKey:  secretKey,
		sessionTok: token,
		client:     &http.Client{Timeout: 2 * time.Minute},
	}, nil
}

// NewS3 exposes the S3 store constructor for tests and embedders.
func NewS3(cfg config.S3StoreConfig) (Store, error) { return newS3Store(cfg) }

type s3Store struct {
	cfg        config.S3StoreConfig
	endpoint   *url.URL
	accessKey  string
	secretKey  string
	sessionTok string
	client     *http.Client
}

func parseEndpoint(raw, region string) (*url.URL, error) {
	if raw == "" {
		return &url.URL{Scheme: "https", Host: "s3." + region + ".amazonaws.com"}, nil
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid s3 endpoint: %w", err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("invalid s3 endpoint %q", raw)
	}
	return u, nil
}

func (s *s3Store) objectURL(key string) *url.URL {
	u := *s.endpoint
	basePath := strings.TrimSuffix(u.Path, "/")
	keyPath := path.Join("/", s.cfg.Prefix, key)
	if s.cfg.PathStyle {
		u.Path = basePath + "/" + s.cfg.Bucket + keyPath
	} else {
		u.Host = s.cfg.Bucket + "." + u.Host
		u.Path = basePath + keyPath
	}
	return &u
}

func (s *s3Store) Put(ctx context.Context, key string, r io.Reader, size int64, digest, contentType string) (string, error) {
	u := s.objectURL(key)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), io.LimitReader(r, size))
	if err != nil {
		return "", err
	}
	req.ContentLength = size
	if contentType == "" {
		contentType = "application/gzip"
	}
	req.Header.Set("Content-Type", contentType)
	signV4(req, digest, s.accessKey, s.secretKey, s.sessionTok, s.cfg.Region, time.Now())
	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("s3 put: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("s3 put: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return "s3://" + s.cfg.Bucket + "/" + path.Join(s.cfg.Prefix, key), nil
}

func (s *s3Store) Get(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	u := s.objectURL(key)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, 0, err
	}
	signV4(req, sha256Hex(nil), s.accessKey, s.secretKey, s.sessionTok, s.cfg.Region, time.Now())
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("s3 get: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		_ = resp.Body.Close()
		return nil, 0, ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		return nil, 0, fmt.Errorf("s3 get: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return resp.Body, resp.ContentLength, nil
}

// signV4 signs req with AWS Signature Version 4 for the S3 service. It signs
// every header except Authorization, User-Agent, and Accept-Encoding.
func signV4(req *http.Request, payloadHash, accessKey, secretKey, sessionToken, region string, now time.Time) {
	now = now.UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	if req.Header == nil {
		req.Header = http.Header{}
	}
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if sessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", sessionToken)
	}

	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	headers := map[string]string{"host": host}
	for name, values := range req.Header {
		lower := strings.ToLower(name)
		switch lower {
		case "authorization", "user-agent", "accept-encoding":
			continue
		}
		trimmed := make([]string, 0, len(values))
		for _, v := range values {
			trimmed = append(trimmed, strings.Join(strings.Fields(v), " "))
		}
		headers[lower] = strings.Join(trimmed, ",")
	}
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)

	var canonicalHeaders strings.Builder
	for _, name := range names {
		canonicalHeaders.WriteString(name)
		canonicalHeaders.WriteString(":")
		canonicalHeaders.WriteString(headers[name])
		canonicalHeaders.WriteString("\n")
	}
	signedHeaders := strings.Join(names, ";")

	uri := req.URL.EscapedPath()
	if uri == "" {
		uri = "/"
	}
	canonicalRequest := strings.Join([]string{
		req.Method,
		uri,
		req.URL.Query().Encode(),
		canonicalHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := dateStamp + "/" + region + "/s3/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	kDate := hmacSHA256([]byte("AWS4"+secretKey), dateStamp)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, "s3")
	kSigning := hmacSHA256(kService, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(kSigning, stringToSign))

	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		accessKey, scope, signedHeaders, signature,
	))
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}
