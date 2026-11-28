// Package e2e holds the end-to-end suite. This file has no build tag so its
// pure helpers run under `go test ./...` without a cluster; the suite itself
// is behind the e2e tag.
package e2e

import (
	"fmt"
	"strings"
	"time"
)

// leftover is one Kubernetes object of a session as the leak check sees it. A
// delete of a persistent volume claim only sets its deletionTimestamp: the
// kubernetes.io/pvc-protection finalizer keeps the object listed until the
// controller clears it. An object with a timestamp is therefore on its way
// out, not leaked.
type leftover struct {
	// Kind is the singular resource kind, as kubectl -o name prints it.
	Kind string
	// Name is the object name.
	Name string
	// Terminating reports that the API server accepted a delete: the object
	// carries a deletionTimestamp.
	Terminating bool
}

// String names the object as kubectl does, kind/name.
func (l leftover) String() string {
	return l.Kind + "/" + l.Name
}

// parseLeftovers reads jsonpath lines of the form "kind/name<TAB>timestamp",
// as the leak check's kubectl command prints them. An empty timestamp means
// the object is not terminating. A blank line is skipped. A line that does not
// fit the form, or a timestamp that is not RFC 3339, is an error, so broken
// output never passes for a terminating object.
func parseLeftovers(out string) ([]leftover, error) {
	var objs []leftover
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		id, stamp, ok := strings.Cut(line, "\t")
		if !ok {
			return nil, fmt.Errorf("leftover line %q has no tab before the deletion timestamp", line)
		}
		kind, name, ok := strings.Cut(id, "/")
		if !ok || kind == "" || name == "" {
			return nil, fmt.Errorf("leftover line %q is not kind/name", line)
		}
		if stamp != "" {
			if _, err := time.Parse(time.RFC3339, stamp); err != nil {
				return nil, fmt.Errorf("leftover %s has an invalid deletion timestamp %q: %w", id, stamp, err)
			}
		}
		objs = append(objs, leftover{Kind: kind, Name: name, Terminating: stamp != ""})
	}
	return objs, nil
}

// unterminatedLeftovers returns kind/name of every object without a
// deletionTimestamp, in input order. A delete was never accepted for such an
// object, so it is a real leak; an object with a timestamp is still being
// removed and the caller waits for it instead.
func unterminatedLeftovers(objs []leftover) []string {
	var leaks []string
	for _, obj := range objs {
		if !obj.Terminating {
			leaks = append(leaks, obj.String())
		}
	}
	return leaks
}
