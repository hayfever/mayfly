package session

import "runtime/debug"

// panicComponent labels a recovered background panic in the control-plane
// metrics. It matches metrics.ComponentSession; this package does not import
// the registry, so the value is repeated here.
const panicComponent = "session"

// recoverDispatch turns a panic in background session work into a log record
// and a counter increment. It runs before the deferred wg.Done in dispatch, so
// a panicking function never leaves Shutdown waiting forever.
//
// dispatch receives only a function, not a session ID, so the log carries the
// stack instead. The stack names the method that panicked and is the only way
// to identify the session without changing every dispatch caller.
func (m *Manager) recoverDispatch() {
	value := recover()
	if value == nil {
		return
	}
	if m.logger != nil {
		m.logger.Error("session background work panicked",
			"panic", value,
			"stack", string(debug.Stack()))
	}
	if o := m.observer.Load(); o != nil {
		if p, ok := (*o).(interface{ Panic(string) }); ok {
			p.Panic(panicComponent)
		}
	}
}
