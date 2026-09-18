package eventlog

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type gatedReadFile struct {
	durableFile
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (f *gatedReadFile) ReadAt(data []byte, offset int64) (int, error) {
	f.once.Do(func() {
		close(f.entered)
		<-f.release
	})
	return f.durableFile.ReadAt(data, offset)
}

func TestAppendCompletesDuringBlockedLogRead(t *testing.T) {
	for _, mode := range []string{"replay", "limit", "through", "record", "session"} {
		t.Run(mode, func(t *testing.T) {
			log, err := Open(filepath.Join(t.TempDir(), "events.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = log.Close(context.Background()) })
			for _, sequence := range []protocol.Cursor{1, 3} {
				if err := log.Append(t.Context(), testEvent(sequence)); err != nil {
					t.Fatal(err)
				}
			}
			gate := &gatedReadFile{durableFile: log.file, entered: make(chan struct{}), release: make(chan struct{})}
			log.file = gate
			release := sync.OnceFunc(func() { close(gate.release) })
			defer release()
			type result struct {
				events []protocol.Event
				more   bool
				err    error
			}
			done := make(chan result, 1)
			go func() {
				var r result
				switch mode {
				case "replay":
					r.events, r.err = log.Replay(t.Context(), 0)
				case "limit":
					r.events, r.more, r.err = log.ReplayLimit(t.Context(), 0, 2)
				case "through":
					r.events, r.more, r.err = log.ReplayThrough(t.Context(), 0, 2, 1)
				case "record":
					record, _, err := log.ReadRecord(t.Context(), 1)
					r.events, r.err = []protocol.Event{record.Event}, err
				case "session":
					r.events, r.more, r.err = log.ReplaySessionBefore(t.Context(), "", []protocol.ThreadID{"thread"}, 0, 3, 2)
				}
				done <- r
			}()
			awaitLogSignal(t, gate.entered)
			appended := make(chan error, 1)
			go func() { appended <- log.Append(t.Context(), testEvent(4)) }()
			select {
			case err := <-appended:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("append waited for blocked ReadAt")
			}
			release()
			r := <-done
			want := []protocol.Cursor{1, 3}
			if mode == "record" || mode == "through" {
				want = []protocol.Cursor{1}
			} else if mode == "session" {
				want = []protocol.Cursor{3, 1}
			}
			if r.err != nil || r.more || !reflect.DeepEqual(eventSequences(r.events), want) {
				t.Fatalf("sequences=%v more=%v err=%v", eventSequences(r.events), r.more, r.err)
			}
		})
	}
}

func TestCloseKeepsBlockedReadFileAlive(t *testing.T) {
	log, err := Open(filepath.Join(t.TempDir(), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(t.Context(), testEvent(1)); err != nil {
		t.Fatal(err)
	}
	gate := &gatedReadFile{durableFile: log.file, entered: make(chan struct{}), release: make(chan struct{})}
	log.file = gate
	release := sync.OnceFunc(func() { close(gate.release) })
	defer release()
	read := make(chan error, 1)
	go func() { _, err := log.Replay(t.Context(), 0); read <- err }()
	awaitLogSignal(t, gate.entered)
	closed := make(chan error, 1)
	go func() { closed <- log.Close(context.Background()) }()
	// The active reader owns the file lifetime even though mu is available.
	if log.readers.TryLock() {
		log.readers.Unlock()
		t.Fatal("blocked ReadAt did not retain file lifetime")
	}
	release()
	if err := <-read; err != nil {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if _, err := log.Replay(t.Context(), 0); !errors.Is(err, ErrClosed) {
		t.Fatalf("replay after close = %v", err)
	}
}

func awaitLogSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal("log read did not reach gate")
	}
}
