package agent

import "testing"

// TestTurnCounterQueuedTurns covers several queued messages: one finished
// turn must not make the session look idle while another is still pending.
func TestTurnCounterQueuedTurns(t *testing.T) {
	var c turnCounter
	c.start()
	c.start()
	if !c.active() {
		t.Fatal("two started turns report inactive")
	}
	c.finish()
	if !c.active() {
		t.Fatal("one remaining turn reports inactive")
	}
	c.finish()
	if c.active() {
		t.Fatal("all turns finished but still active")
	}
	c.finish()
	if c.n != 0 {
		t.Fatalf("counter = %d, want 0 after an extra finish", c.n)
	}
}
