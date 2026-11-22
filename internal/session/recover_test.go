package session

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/hayfever/mayfly/internal/model"
)

// panicObserver records the component of each recovered panic. It also
// satisfies Observer, so SetObserver accepts it.
type panicObserver struct {
	mu         sync.Mutex
	components []string
}

func (*panicObserver) SessionCreated() {}

func (*panicObserver) SessionFinished(model.SessionStatus) {}

func (o *panicObserver) Panic(component string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.components = append(o.components, component)
}

func TestDispatchRecoversPanicAndReleasesShutdown(t *testing.T) {
	var logs bytes.Buffer
	m := &Manager{logger: slog.New(slog.NewTextHandler(&logs, nil))}
	observer := &panicObserver{}
	m.SetObserver(observer)

	if !m.dispatch(func() { panic("background work blew up") }) {
		t.Fatal("dispatch refused work")
	}
	m.wg.Wait()

	text := logs.String()
	for _, want := range []string{
		"session background work panicked",
		`panic="background work blew up"`,
		"stack=",
		"goroutine",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("panic log is missing %q:\n%s", want, text)
		}
	}
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if len(observer.components) != 1 || observer.components[0] != panicComponent {
		t.Fatalf("panic components = %v, want [%s]", observer.components, panicComponent)
	}
}
