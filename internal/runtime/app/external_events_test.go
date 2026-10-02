package app

import (
	"sync"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// A child-agent observer records graph transitions through PublishExternal
// while it is handling an event. Observers must therefore run outside the
// publish locks, and the nested event must still be observed after the event
// that caused it.
func TestObserverMayPublishExternalEventsWithoutDeadlock(t *testing.T) {
	runtime := NewRuntime(Options{
		Engine:           &testEngine{},
		EventStore:       NewMemoryEventStore(16),
		SubscriberBuffer: 8,
	})
	t.Cleanup(func() { closeRuntime(t, runtime) })

	var (
		mu       sync.Mutex
		observed []protocol.EventKind
		nested   error
	)
	remove := runtime.ObserveEvents(func(event protocol.Event) {
		mu.Lock()
		observed = append(observed, event.Kind)
		mu.Unlock()
		if _, ok := event.Data.(*protocol.AgentSpawnedData); !ok {
			return
		}
		err := runtime.PublishExternal(&protocol.AgentStatusData{
			AgentID: "agent-observer", WorkspaceRoot: "/workspace-observer",
			SessionID: "session-observer", Status: "waiting",
		})
		mu.Lock()
		nested = err
		mu.Unlock()
	})
	t.Cleanup(remove)

	done := make(chan error, 1)
	go func() {
		done <- runtime.PublishExternal(&protocol.AgentSpawnedData{
			AgentID: "agent-observer", WorkspaceRoot: "/workspace-observer",
			SessionID: "session-observer", Role: "worker",
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("publishing from an event observer deadlocked")
	}

	mu.Lock()
	defer mu.Unlock()
	if nested != nil {
		t.Fatalf("nested publish: %v", nested)
	}
	want := []protocol.EventKind{protocol.EventAgentSpawned, protocol.EventAgentStatus}
	if len(observed) != len(want) {
		t.Fatalf("observed = %v want %v", observed, want)
	}
	for index := range want {
		if observed[index] != want[index] {
			t.Fatalf("observed = %v want %v", observed, want)
		}
	}
}
