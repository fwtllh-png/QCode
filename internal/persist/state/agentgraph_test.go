package state

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestAppendAgentEventAllocatesSequencesAtomically(t *testing.T) {
	store, err := Open(t.Context(), Options{
		DataDir: t.TempDir(), BusyTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.CloseAll(context.Background()) })

	const count = 32
	errors := make(chan error, count)
	var group sync.WaitGroup
	for index := range count {
		group.Add(1)
		go func() {
			defer group.Done()
			errors <- store.AppendAgentEvent(t.Context(), &protocol.AgentSpawnedData{
				AgentID: fmt.Sprintf("agent-%d", index), WorkspaceRoot: "/workspace",
				SessionID: "session", Role: "explore", Profile: "explore",
				Stance: "read_only",
			})
		}()
	}
	group.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	events, err := store.Replay(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != count {
		t.Fatalf("events = %d, want %d", len(events), count)
	}
}
