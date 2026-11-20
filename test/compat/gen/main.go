// Command gen writes the persisted-format fixture set for one release. It is
// run at release time from the release's own build, and the output is
// committed. Do not regenerate a directory whose release has shipped: later
// builds must keep reading those bytes. See docs/development.md, "Persisted
// formats and migrations".
//
//	go run ./test/compat/gen -out test/compat/testdata/v0.2.0 -force
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/hayfever/mayfly/test/compat"
)

func main() {
	out := flag.String("out", filepath.Join("test", "compat", "testdata", compat.FixtureVersion), "directory for the fixture set")
	force := flag.Bool("force", false, "replace an existing fixture set")
	flag.Parse()
	if err := compat.Generate(*out, *force); err != nil {
		log.Fatal(err)
	}
	if err := report(*out); err != nil {
		log.Fatal(err)
	}
}

func report(dir string) error {
	fmt.Printf("wrote fixtures to %s\n", dir)
	for _, name := range []string{compat.FileQueueBolt, compat.FileRedisDump, compat.FileJournal, compat.FileState, compat.FileRecords} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		fmt.Printf("  %-18s %6d bytes\n", name, info.Size())
	}
	return nil
}
