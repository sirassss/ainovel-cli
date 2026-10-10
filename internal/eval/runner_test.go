package eval

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/voocel/ainovel-cli/internal/host"
)

// fakeEngine simulates the host: after Abort it sends on done once, like waitDone. done has a
// buffer of 1, and the tests use it to assert whether drive drained Done (len(done)==0 means it
// was consumed); this is the key invariant against a send-on-closed-channel panic.
type fakeEngine struct {
	events chan host.Event
	stream chan string
	done   chan struct{}

	mu      sync.Mutex
	snap    host.UISnapshot
	aborted bool
}

func newFakeEngine() *fakeEngine {
	return &fakeEngine{
		events: make(chan host.Event, 4),
		stream: make(chan string),
		done:   make(chan struct{}, 1),
	}
}

func (f *fakeEngine) Events() <-chan host.Event { return f.events }
func (f *fakeEngine) Stream() <-chan string     { return f.stream }
func (f *fakeEngine) Done() <-chan struct{}     { return f.done }

func (f *fakeEngine) Snapshot() host.UISnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snap
}

func (f *fakeEngine) Abort() bool {
	f.mu.Lock()
	f.aborted = true
	f.mu.Unlock()
	select { // simulates waitDone: send on done once after abort fires
	case f.done <- struct{}{}:
	default:
	}
	return true
}

func (f *fakeEngine) wasAborted() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.aborted
}

// The timeout path must Abort, drain to Done and only then return the timeout error; otherwise
// RunCase's Close would race with waitDone over closing the done channel and panic (Codex
// review #1).
func TestDriveTimeoutDrainsToDone(t *testing.T) {
	f := newFakeEngine()
	err := drive(f, 1, RunOptions{Timeout: 30 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("a timeout should return a timeout error, got %v", err)
	}
	if !f.wasAborted() {
		t.Fatal("a timeout should trigger Abort")
	}
	if len(f.done) != 0 {
		t.Fatal("drive must drain Done before returning (otherwise it races with Close over closing the channel and panics)")
	}
}

// Chapter cap reached: Abort, drain to Done, and return nil (a normal cap stop, not a timeout).
func TestDriveCapStopsAndDrains(t *testing.T) {
	f := newFakeEngine()
	f.mu.Lock()
	f.snap = host.UISnapshot{CompletedCount: 1}
	f.mu.Unlock()
	f.events <- host.Event{Category: "SYSTEM", Summary: "committed"} // triggers the cap check

	err := drive(f, 1, RunOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("a normal cap stop should return nil, got %v", err)
	}
	if !f.wasAborted() {
		t.Fatal("reaching the chapter cap should Abort")
	}
	if len(f.done) != 0 {
		t.Fatal("should return only after draining Done")
	}
}

// The engine reaches Done naturally (writing finished): no Abort needed, returns nil.
func TestDriveNaturalDoneReturnsNil(t *testing.T) {
	f := newFakeEngine()
	f.done <- struct{}{}

	err := drive(f, 1, RunOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("a natural finish should return nil, got %v", err)
	}
	if f.wasAborted() {
		t.Fatal("a natural finish should not Abort")
	}
}
