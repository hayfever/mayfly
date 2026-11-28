// Package release checks that the release metadata files agree on one
// version. A tag publishes the chart and the API document together, so a
// mismatch in this repository is a release defect that no other test covers.
package release

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const repoRoot = "../.."

// datedHeading matches a released CHANGELOG heading such as
// "## [1.0.0-rc.4] - 2026-11-27". The link references at the bottom of the
// file do not match it.
var datedHeading = regexp.MustCompile(`^## \[([^\]]+)\] - \d{4}-\d{2}-\d{2}$`)

type chartMetadata struct {
	Version    string `yaml:"version"`
	AppVersion string `yaml:"appVersion"`
}

type apiDocument struct {
	Info struct {
		Version string `yaml:"version"`
	} `yaml:"info"`
}

// checkVersions reports the first disagreement between the release metadata
// values.
func checkVersions(chartVersion, appVersion, apiVersion, changelogVersion string) error {
	for _, field := range []struct{ name, value string }{
		{"chart version", chartVersion},
		{"chart appVersion", appVersion},
		{"openapi info.version", apiVersion},
		{"newest CHANGELOG heading", changelogVersion},
	} {
		if field.value == "" {
			return fmt.Errorf("%s is empty", field.name)
		}
	}
	want := changelogVersion
	for _, field := range []struct{ name, value string }{
		{"chart version", chartVersion},
		{"chart appVersion", appVersion},
		{"openapi info.version", apiVersion},
	} {
		if field.value != want {
			return fmt.Errorf("%s is %q, want %q from the newest CHANGELOG heading", field.name, field.value, want)
		}
	}
	return nil
}

func readYAML(path string, out any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return yaml.Unmarshal(data, out)
}

func readChangelog(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if match := datedHeading.FindStringSubmatch(strings.TrimSpace(line)); match != nil {
			return match[1], nil
		}
	}
	return "", fmt.Errorf("no dated release heading in %s", path)
}

// TestReleaseMetadataAgrees is the gate for a release commit: the chart, the
// API document, and the newest dated CHANGELOG entry name the same version.
func TestReleaseMetadataAgrees(t *testing.T) {
	var chart chartMetadata
	if err := readYAML(filepath.Join(repoRoot, "deploy", "helm", "mayfly", "Chart.yaml"), &chart); err != nil {
		t.Fatalf("read chart: %v", err)
	}
	var api apiDocument
	if err := readYAML(filepath.Join(repoRoot, "api", "openapi.yaml"), &api); err != nil {
		t.Fatalf("read openapi: %v", err)
	}
	changelog, err := readChangelog(filepath.Join(repoRoot, "CHANGELOG.md"))
	if err != nil {
		t.Fatalf("read changelog: %v", err)
	}
	if err := checkVersions(chart.Version, chart.AppVersion, api.Info.Version, changelog); err != nil {
		t.Fatal(err)
	}
}

func TestCheckVersionsRejectsMismatch(t *testing.T) {
	cases := []struct {
		name                       string
		chart, app, api, changelog string
		wantErr                    bool
	}{
		{"agree", "1.0.0-rc.4", "1.0.0-rc.4", "1.0.0-rc.4", "1.0.0-rc.4", false},
		{"chart differs", "1.0.0-rc.3", "1.0.0-rc.4", "1.0.0-rc.4", "1.0.0-rc.4", true},
		{"appVersion differs", "1.0.0-rc.4", "1.0.0-rc.3", "1.0.0-rc.4", "1.0.0-rc.4", true},
		{"api differs", "1.0.0-rc.4", "1.0.0-rc.4", "1.0.0-rc.3", "1.0.0-rc.4", true},
		{"changelog behind", "1.0.0-rc.4", "1.0.0-rc.4", "1.0.0-rc.4", "1.0.0-rc.3", true},
		{"empty version", "", "", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkVersions(tc.chart, tc.app, tc.api, tc.changelog)
			if tc.wantErr && err == nil {
				t.Fatal("got nil error, want mismatch")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("got %v, want nil", err)
			}
		})
	}
}
