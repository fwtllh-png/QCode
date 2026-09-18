package assembly

import (
	"io"
	"sync"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
)

// MaxCoalescedDeltaBytes is the payload budget for one waiting batch. It
// preserves the existing 1 KiB batching budget, but is never a flush target:
// Recv delivers available output immediately. A larger provider event is
// delivered intact; the reader retains at most one additional source event.
const MaxCoalescedDeltaBytes = 1024

type streamResult struct {
	event provider.StreamEvent
	err   error
}

type DeltaCoalescingStream struct {
	source    provider.Stream
	startOnce sync.Once
	closeOnce sync.Once
	closeErr  error
	observe   func()
	mu        sync.Mutex
	changed   *sync.Cond
	pending   *streamResult
	closed    bool
	finished  bool
}

func NewDeltaCoalescingStream(
	source provider.Stream,
	observe ...func(),
) provider.Stream {
	stream := &DeltaCoalescingStream{
		source: source,
	}
	stream.changed = sync.NewCond(&stream.mu)
	if len(observe) != 0 {
		stream.observe = observe[0]
	}
	return stream
}

func (s *DeltaCoalescingStream) Recv() (provider.StreamEvent, error) {
	// The consumer must finish transport setup and its initial checkpoint
	// before any provider output is read or observed.
	s.startOnce.Do(func() { go s.read() })
	s.mu.Lock()
	defer s.mu.Unlock()
	for s.pending == nil && !s.closed && !s.finished {
		s.changed.Wait()
	}
	if s.closed || s.pending == nil {
		return provider.StreamEvent{}, io.EOF
	}
	result := *s.pending
	s.pending = nil
	s.changed.Broadcast()
	return result.event, result.err
}

func (s *DeltaCoalescingStream) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.pending = nil
		s.changed.Broadcast()
		s.mu.Unlock()
		s.closeErr = s.source.Close()
	})
	return s.closeErr
}

func (s *DeltaCoalescingStream) read() {
	for {
		s.mu.Lock()
		closed := s.closed
		s.mu.Unlock()
		if closed {
			return
		}
		event, err := s.source.Recv()
		if err == nil && s.observe != nil && outputBearingEvent(event) {
			s.observe()
		}
		if !s.offer(streamResult{event: event, err: err}) ||
			err != nil || event.Type == provider.EventMessageStop {
			return
		}
	}
}

// offer combines only output already waiting for a busy consumer. Boundaries
// and a full batch stop the reader until Recv makes room; there is no timer,
// unbounded queue, or wait for a future fragment on the consumer path.
func (s *DeltaCoalescingStream) offer(result streamResult) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for !s.closed {
		if s.pending == nil {
			event := result.event
			if event.ToolCall != nil {
				fragment := *event.ToolCall
				event.ToolCall = &fragment
			}
			if event.Block != nil {
				block := *event.Block
				event.Block = &block
			}
			result.event = event
			s.pending = &result
			s.finished = result.err != nil || event.Type == provider.EventMessageStop
			s.changed.Broadcast()
			return true
		}
		buffered := &s.pending.event
		if s.pending.err == nil && result.err == nil &&
			coalescibleDelta(*buffered) && coalescibleDelta(result.event) &&
			buffered.Type == result.event.Type &&
			deltaIndex(*buffered) == deltaIndex(result.event) &&
			compatibleDelta(buffered, result.event) &&
			deltaSize(result.event) <= MaxCoalescedDeltaBytes &&
			deltaSize(*buffered) <= MaxCoalescedDeltaBytes-deltaSize(result.event) {
			mergeDelta(buffered, result.event)
			return true
		}
		s.changed.Wait()
	}
	return false
}

func outputBearingEvent(event provider.StreamEvent) bool {
	return event.Type == provider.EventTextDelta ||
		event.Type == provider.EventReasoningDelta ||
		event.Type == provider.EventSearchResult ||
		event.Type == provider.EventCitation ||
		event.Type == provider.EventToolCallDelta
}

func coalescibleDelta(event provider.StreamEvent) bool {
	if event.Type == provider.EventTextDelta ||
		event.Type == provider.EventReasoningDelta {
		return event.Text != ""
	}
	if event.Type == provider.EventToolCallDelta {
		return event.ToolCall != nil
	}
	return false
}

func deltaIndex(event provider.StreamEvent) int {
	if event.Type == provider.EventToolCallDelta && event.ToolCall != nil {
		return event.ToolCall.Index
	}
	return event.Index
}

func compatibleDelta(
	buffered *provider.StreamEvent,
	next provider.StreamEvent,
) bool {
	if buffered.Type != provider.EventToolCallDelta {
		return true
	}
	current := buffered.ToolCall
	incoming := next.ToolCall
	if current == nil || incoming == nil {
		return false
	}
	return (current.ID == "" || incoming.ID == "" || current.ID == incoming.ID) &&
		(current.Name == "" || incoming.Name == "" || current.Name == incoming.Name)
}

func deltaSize(event provider.StreamEvent) int {
	if event.Type == provider.EventToolCallDelta && event.ToolCall != nil {
		return len(event.ToolCall.Arguments)
	}
	return len(event.Text)
}

func mergeDelta(buffered *provider.StreamEvent, next provider.StreamEvent) {
	if buffered.Type == provider.EventToolCallDelta {
		if buffered.ToolCall.ID == "" {
			buffered.ToolCall.ID = next.ToolCall.ID
		}
		if buffered.ToolCall.Name == "" {
			buffered.ToolCall.Name = next.ToolCall.Name
		}
		buffered.ToolCall.Arguments += next.ToolCall.Arguments
		return
	}
	buffered.Text += next.Text
	if buffered.Block != nil && next.Block != nil {
		buffered.Block.Text += next.Block.Text
	}
}
