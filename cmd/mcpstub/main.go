// Command mcpstub is a minimal Streamable HTTP MCP server for local
// development and end-to-end tests. It exposes one echo tool.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/hayden/mayfly/internal/stub"
)

func main() {
	addr := flag.String("addr", ":9100", "listen address")
	flag.Parse()
	log.Printf("mcpstub listening on %s", *addr)
	if err := http.ListenAndServe(*addr, stub.MCPHandler()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
