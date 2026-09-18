package assembly

import (
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
)

func TestDeltaCoalescingStreamBatchesUnderBackpressure(t *testing.T) {
	for _, kind := range []provider.StreamEventType{
		provider.EventTextDelta, provider.EventReasoningDelta, provider.EventToolCallDelta,
	} {
		t.Run(string(kind), func(t *testing.T) {
			const count = 10_000
			events := make([]provider.StreamEvent, 0, count+1)
			for range count {
				event := provider.StreamEvent{Type: kind, Text: "x",
					Block: &provider.ContentBlock{Type: provider.ContentText, Text: "x"}}
				if kind == provider.EventToolCallDelta {
					event = provider.StreamEvent{Type: kind, ToolCall: &provider.ToolCallFragment{
						ID: "call_1", Name: "read", Arguments: "x",
					}}
				}
				events = append(events, event)
			}
			events = append(events, provider.StreamEvent{Type: provider.EventMessageStop})
			source := newNotifyingDeltaStream(events)
			stream := startDeltaStream(t, source)
			defer stream.Close()

			var text strings.Builder
			var batches int
			for text.Len() < count {
				size := min(MaxCoalescedDeltaBytes, count-text.Len())
				// The next source read proves all preceding fragments have
				// reached the waiting batch. No scheduler-dependent sleeps.
				source.waitReads(t, text.Len()+size+1)
				event, err := stream.Recv()
				if err != nil {
					t.Fatal(err)
				}
				if event.Type != kind || deltaSize(event) != size {
					t.Fatalf("batch = %+v, want %s with %d bytes", event, kind, size)
				}
				if kind == provider.EventToolCallDelta {
					text.WriteString(event.ToolCall.Arguments)
				} else {
					text.WriteString(event.Text)
					if event.Block.Text != event.Text {
						t.Fatalf("block text differs from delta: %+v", event)
					}
				}
				batches++
			}
			if text.String() != strings.Repeat("x", count) || batches >= count/20 {
				t.Fatalf("text bytes = %d, batches = %d", text.Len(), batches)
			}
			remaining, err := provider.Drain(stream)
			if err != nil || len(remaining) != 1 || remaining[0].Type != provider.EventMessageStop {
				t.Fatalf("remaining = %+v, err = %v", remaining, err)
			}
			for _, event := range events[:count] {
				if deltaSize(event) != 1 || (event.Block != nil && event.Block.Text != "x") {
					t.Fatalf("source event was mutated: %+v", event)
				}
			}
		})
	}
}

func TestDeltaCoalescingStreamPreservesBoundaries(t *testing.T) {
	tool := func(index int, id, name, args string) provider.StreamEvent {
		return provider.StreamEvent{Type: provider.EventToolCallDelta,
			ToolCall: &provider.ToolCallFragment{Index: index, ID: id, Name: name, Arguments: args}}
	}
	events := []provider.StreamEvent{
		{Type: provider.EventReasoningDelta, Text: "a"},
		{Type: provider.EventReasoningDelta, Text: "b"},
		{Type: provider.EventReasoningDelta, Index: 1, Text: "c"},
		tool(0, "call_1", "read", `{"path":`),
		tool(0, "", "", `"one"}`),
		tool(1, "call_2", "read", "{}"),
		tool(1, "call_3", "read", "{}"),
		tool(1, "call_3", "write", "{}"),
		{Type: provider.EventTextDelta, Text: "d"},
		{Type: provider.EventTextDelta, Text: "e"},
		{Type: provider.EventUsage, Usage: &provider.Usage{InputTokens: 1}},
		{Type: provider.EventMessageStop},
	}
	batches := []struct {
		last int
		want provider.StreamEvent
	}{
		{2, provider.StreamEvent{Type: provider.EventReasoningDelta, Text: "ab"}},
		{3, events[2]},
		{5, tool(0, "call_1", "read", `{"path":"one"}`)},
		{6, events[5]},
		{7, events[6]},
		{8, events[7]},
		{10, provider.StreamEvent{Type: provider.EventTextDelta, Text: "de"}},
		{11, events[10]},
		{12, events[11]},
	}
	source := newNotifyingDeltaStream(events)
	stream := startDeltaStream(t, source)
	defer stream.Close()
	for _, batch := range batches {
		if batch.want.Type != provider.EventMessageStop {
			source.waitReads(t, batch.last+1)
		}
		got, err := stream.Recv()
		if err != nil || !reflect.DeepEqual(got, batch.want) {
			t.Fatalf("Recv = %+v, %v; want %+v", got, err, batch.want)
		}
	}
	if _, err := stream.Recv(); err != io.EOF {
		t.Fatalf("terminal error = %v", err)
	}
}

func TestDeltaCoalescingStreamByteBudget(t *testing.T) {
	for _, size := range []int{MaxCoalescedDeltaBytes - 1, MaxCoalescedDeltaBytes, MaxCoalescedDeltaBytes + 1} {
		events := []provider.StreamEvent{
			{Type: provider.EventTextDelta, Text: strings.Repeat("x", size)},
			{Type: provider.EventTextDelta, Text: "y"},
		}
		source := newNotifyingDeltaStream(events)
		stream := startDeltaStream(t, source)
		if size < MaxCoalescedDeltaBytes {
			source.waitReads(t, 3)
		} else {
			source.waitReads(t, 2)
		}
		event, err := stream.Recv()
		want := events[0].Text
		if size < MaxCoalescedDeltaBytes {
			want += "y"
		}
		if err != nil || event.Text != want {
			t.Fatalf("size %d: got %d bytes, err = %v", size, len(event.Text), err)
		}
		rest, err := provider.Drain(stream)
		_ = stream.Close()
		if err != nil || (size < MaxCoalescedDeltaBytes && len(rest) != 0) ||
			(size >= MaxCoalescedDeltaBytes && (len(rest) != 1 || rest[0].Text != "y")) {
			t.Fatalf("size %d: remaining = %+v, err = %v", size, rest, err)
		}
	}
}

func TestDeltaCoalescingStreamDeliversEveryDeltaWhenProviderPauses(t *testing.T) {
	source := newControlledDeltaStream()
	stream := NewDeltaCoalescingStream(source)
	defer stream.Close()
	for _, text := range []string{"first", "second", "third"} {
		// Leave the following provider read blocked. Even a non-first small
		// delta must arrive before the former 250 ms batching window.
		got := make(chan streamResult, 1)
		go func() {
			event, err := stream.Recv()
			got <- streamResult{event: event, err: err}
		}()
		request := source.nextRequest(t)
		request <- streamResult{event: provider.StreamEvent{Type: provider.EventTextDelta, Text: text}}
		select {
		case result := <-got:
			if result.err != nil || result.event.Text != text {
				t.Fatalf("Recv = %+v", result)
			}
		case <-time.After(200 * time.Millisecond):
			t.Fatal("available output waited for a future provider fragment")
		}
	}
}

func TestDeltaCoalescingStreamDrainsBeforeError(t *testing.T) {
	cause := errors.New("provider disconnected")
	source := newNotifyingDeltaStream([]provider.StreamEvent{
		{Type: provider.EventTextDelta, Text: "a"},
		{Type: provider.EventTextDelta, Text: "b"},
	})
	source.err = cause
	stream := startDeltaStream(t, source)
	defer stream.Close()
	source.waitReads(t, 3)
	event, err := stream.Recv()
	if err != nil || event.Text != "ab" {
		t.Fatalf("buffered event = %+v, %v", event, err)
	}
	if _, err := stream.Recv(); !errors.Is(err, cause) {
		t.Fatalf("source error = %v", err)
	}
	if _, err := stream.Recv(); err != io.EOF {
		t.Fatalf("repeated terminal read = %v", err)
	}
}

func TestDeltaCoalescingStreamCloseWakesReaderAndConsumer(t *testing.T) {
	source := newControlledDeltaStream()
	stream := NewDeltaCoalescingStream(source)
	done := make(chan error, 1)
	go func() {
		_, err := stream.Recv()
		done <- err
	}()
	source.nextRequest(t) // Source is blocked in Recv.
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != io.EOF {
			t.Fatalf("closed Recv = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not wake Recv")
	}
	select {
	case <-source.exited:
	case <-time.After(time.Second):
		t.Fatal("Close did not release the source")
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDeltaCoalescingStreamCloseWakesBackpressuredProducer(t *testing.T) {
	// Drive the handoff directly to make producer blocking deterministic.
	stream := &DeltaCoalescingStream{
		source: newNotifyingDeltaStream(nil),
		pending: &streamResult{event: provider.StreamEvent{
			Type: provider.EventTextDelta, Text: strings.Repeat("x", MaxCoalescedDeltaBytes),
		}},
	}
	stream.changed = sync.NewCond(&stream.mu)
	done := make(chan bool, 1)
	go func() {
		done <- stream.offer(streamResult{event: provider.StreamEvent{Type: provider.EventTextDelta, Text: "y"}})
	}()
	_ = stream.Close()
	select {
	case offered := <-done:
		if offered {
			t.Fatal("producer published after Close")
		}
	case <-time.After(time.Second):
		t.Fatal("producer remained blocked after Close")
	}
}

type notifyingDeltaStream struct {
	events []provider.StreamEvent
	index  int
	calls  chan int
	err    error
	start  bool
}

func newNotifyingDeltaStream(events []provider.StreamEvent) *notifyingDeltaStream {
	return &notifyingDeltaStream{events: events, calls: make(chan int, len(events)+1), err: io.EOF, start: true}
}

func startDeltaStream(t *testing.T, source *notifyingDeltaStream) provider.Stream {
	t.Helper()
	stream := NewDeltaCoalescingStream(source)
	event, err := stream.Recv()
	if err != nil || event.Type != provider.EventMessageStart {
		t.Fatalf("start event = %+v, %v", event, err)
	}
	return stream
}

func (s *notifyingDeltaStream) Recv() (provider.StreamEvent, error) {
	if s.start {
		s.start = false
		return provider.StreamEvent{Type: provider.EventMessageStart}, nil
	}
	s.calls <- s.index + 1
	if s.index == len(s.events) {
		return provider.StreamEvent{}, s.err
	}
	event := s.events[s.index]
	s.index++
	return event, nil
}

func (s *notifyingDeltaStream) Close() error { return nil }

func (s *notifyingDeltaStream) waitReads(t *testing.T, want int) {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case count := <-s.calls:
			if count == want {
				return
			}
		case <-timer.C:
			t.Fatalf("provider did not reach read %d", want)
		}
	}
}

type controlledDeltaStream struct {
	requests chan chan streamResult
	closed   chan struct{}
	exited   chan struct{}
	once     sync.Once
}

func newControlledDeltaStream() *controlledDeltaStream {
	return &controlledDeltaStream{
		requests: make(chan chan streamResult), closed: make(chan struct{}), exited: make(chan struct{}),
	}
}

func (s *controlledDeltaStream) Recv() (provider.StreamEvent, error) {
	reply := make(chan streamResult)
	select {
	case s.requests <- reply:
	case <-s.closed:
		close(s.exited)
		return provider.StreamEvent{}, io.EOF
	}
	select {
	case result := <-reply:
		return result.event, result.err
	case <-s.closed:
		close(s.exited)
		return provider.StreamEvent{}, io.EOF
	}
}

func (s *controlledDeltaStream) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

func (s *controlledDeltaStream) nextRequest(t *testing.T) chan streamResult {
	t.Helper()
	select {
	case request := <-s.requests:
		return request
	case <-time.After(time.Second):
		t.Fatal("source was not read")
		return nil
	}
}
