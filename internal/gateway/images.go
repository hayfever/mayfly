package gateway

import (
	"fmt"
	"sync"

	"github.com/hayfever/mayfly/internal/content"
	"github.com/hayfever/mayfly/internal/journal"
	"github.com/hayfever/mayfly/internal/mcp"
)

// admitKey names one ledger call whose image bytes were already charged.
type admitKey struct {
	session string
	call    string
}

// imageAdmission bounds the image bytes one session may store. A repeated
// complete for the same ledger key returns the first decision, so a retry
// does not charge the session twice.
type imageAdmission struct {
	mu        sync.Mutex
	limits    content.Limits
	budgets   map[string]*content.Budget
	decisions map[admitKey]bool
}

func newImageAdmission(limits content.Limits) *imageAdmission {
	return &imageAdmission{
		limits:    limits.WithDefaults(),
		budgets:   map[string]*content.Budget{},
		decisions: map[admitKey]bool{},
	}
}

// admit reports whether n image bytes of one call fit the session budget.
func (a *imageAdmission) admit(session, call string, n int64) bool {
	if n <= 0 {
		return true
	}
	key := admitKey{session: session, call: call}
	a.mu.Lock()
	defer a.mu.Unlock()
	if admitted, ok := a.decisions[key]; ok {
		return admitted
	}
	budget := a.budgets[session]
	if budget == nil {
		budget = content.NewBudget(a.limits.MaxSessionImageBytes, 0)
		a.budgets[session] = budget
	}
	admitted := budget.Accept(n)
	a.decisions[key] = admitted
	return admitted
}

// drop forgets one session's budget and decisions.
func (a *imageAdmission) drop(session string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.budgets, session)
	for key := range a.decisions {
		if key.session == session {
			delete(a.decisions, key)
		}
	}
}

// admissibleResult validates and charges one result's images. An invalid or
// over-budget image becomes a text error, and the result is marked an error:
// the model must not see a result that silently lost content.
func (g *Gateway) admissibleResult(session, call string, res mcp.ToolResult) mcp.ToolResult {
	if !res.HasImage() {
		return res
	}
	parts, bytes, rejected := content.Sanitize(res.Parts, g.images.limits)
	if rejected {
		res.IsError = true
	}
	if content.HasImage(parts) && !g.images.admit(session, call, bytes) {
		parts = content.WithoutImages(parts, fmt.Sprintf(
			"the session image limit of %d bytes is reached", g.images.limits.MaxSessionImageBytes))
		res.IsError = true
	}
	res.Parts = parts
	return res
}

// recordedResult converts one admitted result into the ledger record. A
// text-only result keeps the plain string shape. An image result stores its
// parts, with text capped by max so a large text block cannot crowd the
// ledger.
func recordedResult(res mcp.ToolResult, max int) journal.Result {
	if !res.HasImage() {
		return journal.Result{Content: truncate(res.Text(), max), IsError: res.IsError}
	}
	parts := content.TruncateText(res.Parts, max)
	return journal.Result{Content: truncate(content.Text(parts), max), Parts: parts, IsError: res.IsError}
}
