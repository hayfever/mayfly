package config

import (
	"fmt"
	"net/url"
	"path"
	"strings"
)

// repoArchiveURL converts a supported code hosting repository URL and ref into
// a tar.gz archive URL. Other hosts must use url directly.
func repoArchiveURL(repo, ref string) (string, error) {
	repo = strings.TrimSpace(repo)
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", fmt.Errorf("ref is required with repo")
	}
	if !strings.Contains(repo, "://") {
		repo = "https://" + repo
	}
	parsed, err := url.Parse(repo)
	if err != nil {
		return "", fmt.Errorf("invalid repo URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("repo URL must be http or https")
	}
	host := strings.ToLower(parsed.Hostname())
	repoPath := strings.Trim(strings.TrimSuffix(parsed.Path, ".git"), "/")
	if repoPath == "" {
		return "", fmt.Errorf("repo URL has no path")
	}
	if strings.Contains(ref, "..") || strings.ContainsAny(ref, " \t\r\n") {
		return "", fmt.Errorf("invalid ref %q", ref)
	}
	escapedRef := url.PathEscape(ref)
	switch host {
	case "github.com":
		return fmt.Sprintf("%s://%s/%s/archive/%s.tar.gz", parsed.Scheme, parsed.Host, repoPath, escapedRef), nil
	case "gitlab.com":
		return fmt.Sprintf("%s://%s/%s/-/archive/%s/%s-%s.tar.gz", parsed.Scheme, parsed.Host, repoPath, escapedRef, path.Base(repoPath), escapedRef), nil
	case "bitbucket.org":
		return fmt.Sprintf("%s://%s/%s/get/%s.tar.gz", parsed.Scheme, parsed.Host, repoPath, escapedRef), nil
	default:
		return "", fmt.Errorf("unsupported repo host %q; set url to a tar.gz archive", host)
	}
}
