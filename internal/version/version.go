// Package version holds build identity.
package version

import "fmt"

// Set at build time with -ldflags. See the Makefile and release workflow.
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// String returns a human-readable build identity.
func String() string {
	return fmt.Sprintf("%s (commit %s, built %s)", Version, Commit, Date)
}
