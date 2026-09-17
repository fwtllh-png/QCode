package eventlog

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type countingReadFile struct {
	durableFile
	reads int
}

func (f *countingReadFile) ReadAt(data []byte, offset int64) (int, error) {
	f.reads++
	return f.durableFile.ReadAt(data, offset)
}

func TestReplaySessionBeforeIndexesOwnershipAndVerifiesOnlyPage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	log, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for sequence := protocol.Cursor(1); sequence <= 100; sequence++ {
		event := testEvent(sequence)
		event.ThreadID = "foreign"
		if sequence%10 == 0 {
			event.ThreadID = "main"
		}
		if sequence == 85 || sequence == 90 {
			event.Kind = protocol.EventAgentStatus
			owner := "session"
			if sequence == 90 {
				owner = "other"
			}
			event.Data = &protocol.AgentStatusData{
				AgentID: "agent", SessionID: owner, WorkspaceRoot: "/workspace", Status: "running",
			}
		}
		if err := log.Append(t.Context(), event); err != nil {
			t.Fatal(err)
		}
	}
	if err := log.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Startup reconstructs the same ownership index from validated records.
	log, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close(context.Background()) })
	counter := &countingReadFile{durableFile: log.file}
	log.file = counter
	page, more, err := log.ReplaySessionBefore(t.Context(), "session",
		[]protocol.ThreadID{"main", "main"}, 96, 95, 3)
	if err != nil || !more || !reflect.DeepEqual(eventSequences(page), []protocol.Cursor{85, 80, 70}) {
		t.Fatalf("page=%v more=%v err=%v", eventSequences(page), more, err)
	}
	if counter.reads != 3 {
		t.Fatalf("decoded %d records for a three-event page", counter.reads)
	}
	page, more, err = log.ReplaySessionBefore(t.Context(), "session",
		[]protocol.ThreadID{"main"}, 70, 95, 10)
	if err != nil || more || !reflect.DeepEqual(eventSequences(page), []protocol.Cursor{60, 50, 40, 30, 20, 10}) {
		t.Fatalf("older page=%v more=%v err=%v", eventSequences(page), more, err)
	}
	// Indexed reads still verify the committed log bytes.
	evidence, _ := log.Evidence(85)
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte("!"), evidence.Offset); err != nil {
		t.Fatal(err)
	}
	file.Close()
	if _, _, err := log.ReplaySessionBefore(t.Context(), "session",
		[]protocol.ThreadID{"main"}, 96, 95, 1); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corruption not detected: %v", err)
	}
}
