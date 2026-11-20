// Command apidoc renders api/openapi.yaml into the Markdown API reference.
//
// Usage:
//
//	go run ./hack/apidoc                  # write docs/api-reference.md
//	go run ./hack/apidoc -check           # exit 1 when the file is stale
//	go run ./hack/apidoc -spec FILE -out FILE
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"

	"github.com/hayfever/mayfly/internal/apidoc"
)

func main() {
	specPath := flag.String("spec", "api/openapi.yaml", "path of the OpenAPI document")
	outPath := flag.String("out", "docs/api-reference.md", "path of the generated Markdown file")
	check := flag.Bool("check", false, "report a stale output and exit 1 instead of writing")
	flag.Parse()

	if err := run(*specPath, *outPath, *check); err != nil {
		fmt.Fprintln(os.Stderr, "apidoc:", err)
		os.Exit(1)
	}
}

func run(specPath, outPath string, check bool) error {
	spec, err := os.ReadFile(specPath)
	if err != nil {
		return err
	}
	rendered, err := apidoc.Render(spec)
	if err != nil {
		return fmt.Errorf("%s: %w", specPath, err)
	}

	if !check {
		if err := os.WriteFile(outPath, rendered, 0o644); err != nil {
			return err
		}
		fmt.Printf("wrote %s\n", outPath)
		return nil
	}

	current, err := os.ReadFile(outPath)
	if err != nil {
		return fmt.Errorf("%s: %w (run `go run ./hack/apidoc` to create it)", outPath, err)
	}
	if bytes.Equal(current, rendered) {
		fmt.Printf("%s matches %s\n", outPath, specPath)
		return nil
	}
	return fmt.Errorf("%s is stale; run `go run ./hack/apidoc`\n\n%s",
		outPath, apidoc.UnifiedDiff(outPath, outPath, current, rendered))
}
