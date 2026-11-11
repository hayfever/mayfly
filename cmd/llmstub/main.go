// Command llmstub is a deterministic OpenAI-compatible server for local
// development and end-to-end tests. It is not a model.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/hayfever/mayfly/internal/stub"
)

func main() {
	addr := flag.String("addr", ":9099", "listen address")
	flag.Parse()
	log.Printf("llmstub listening on %s", *addr)
	if err := http.ListenAndServe(*addr, stub.LLMHandler()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
