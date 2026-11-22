package sources

import (
	"strings"
	"testing"
)

func TestFanoutRecoversPanicAsItemErrors(t *testing.T) {
	s := &httpJSON{def: Definition{Name: "pages"}}
	jobs := []fanoutJob{
		{url: "https://example.test/a", field: "a"},
		{url: "https://example.test/b", field: "b"},
	}
	out := make([]Document, len(jobs))
	fail := func(message string) {
		for i := range jobs {
			out[i] = s.errorDocument(jobs[i], message)
		}
	}

	// This is the deferred helper the fan-out goroutine defers. If it did not
	// recover, this test binary would exit.
	func() {
		defer s.recoverFanoutTarget(fail)
		panic("projection blew up")
	}()

	for i, document := range out {
		if document.Status != "error" {
			t.Fatalf("document %d status = %q, want error", i, document.Status)
		}
		if !strings.Contains(document.Error, "internal error") || !strings.Contains(document.Error, "projection blew up") {
			t.Fatalf("document %d error = %q", i, document.Error)
		}
		if len(document.Error) > 300 {
			t.Fatalf("document %d error is %d bytes, want at most 300", i, len(document.Error))
		}
	}
}
