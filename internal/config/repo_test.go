package config

import "testing"

func TestRepoArchiveURL(t *testing.T) {
	cases := []struct {
		repo, ref, want string
	}{
		{"https://github.com/acme/skills", "abc123", "https://github.com/acme/skills/archive/abc123.tar.gz"},
		{"github.com/acme/skills", "v1.0.0", "https://github.com/acme/skills/archive/v1.0.0.tar.gz"},
		{"https://gitlab.com/acme/skills", "abc123", "https://gitlab.com/acme/skills/-/archive/abc123/skills-abc123.tar.gz"},
		{"https://bitbucket.org/acme/skills", "abc123", "https://bitbucket.org/acme/skills/get/abc123.tar.gz"},
	}
	for _, tc := range cases {
		got, err := repoArchiveURL(tc.repo, tc.ref)
		if err != nil {
			t.Fatalf("%s: %v", tc.repo, err)
		}
		if got != tc.want {
			t.Fatalf("%s: got %s want %s", tc.repo, got, tc.want)
		}
	}
	if _, err := repoArchiveURL("https://example.com/acme/skills", "abc"); err == nil {
		t.Fatal("expected unsupported host error")
	}
	if _, err := repoArchiveURL("https://github.com/acme/skills", ""); err == nil {
		t.Fatal("expected ref error")
	}
}
