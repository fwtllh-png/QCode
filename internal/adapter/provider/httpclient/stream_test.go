package httpclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestClientCancellationClosesResponse(t *testing.T) {
	requestCanceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		if flusher, ok := writer.(http.Flusher); ok {
			flusher.Flush()
		}
		<-request.Context().Done()
		close(requestCanceled)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	client := testClient()
	stream, err := client.Stream(ctx, testRequest(t, server.URL, model.ProtocolOpenAIChat))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	_, err = stream.Recv()
	if err != nil {
		t.Fatalf("first event error = %v", err)
	}
	_, err = stream.Recv()
	if err == nil {
		t.Fatal("Recv() error = nil after cancellation")
	}
	select {
	case <-requestCanceled:
	case <-time.After(time.Second):
		t.Fatal("server request context was not canceled")
	}
	_ = stream.Close()
}

func TestClientDoesNotReplayAfterStreamStarts(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: {\n\n")
	}))
	defer server.Close()

	client := testClient()
	stream, err := client.Stream(t.Context(), testRequest(t, server.URL, model.ProtocolOpenAIChat))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Drain(stream); err == nil {
		t.Fatal("Drain() error = nil, want malformed stream error")
	}
	if attempts.Load() != 1 {
		t.Fatalf("attempts = %d, want no replay after stream start", attempts.Load())
	}
}

func TestNormalizeStreamErrorClassifiesConnectionReset(t *testing.T) {
	err := normalizeStreamError(
		fmt.Errorf("read stream: %w", syscall.ECONNRESET),
		provider.TransportMetadata{LogicalRequestID: "sample-1"},
	)
	if !protocol.IsCode(err, protocol.CodeUnavailable) ||
		!protocol.IsRetryable(err) ||
		!errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("normalizeStreamError() = %#v", err)
	}
	problem := protocol.ProblemOf(err)
	if problem.Fault == nil ||
		problem.Fault.Stage != protocol.FaultStageModelSample ||
		problem.Fault.OperationID != "sample-1" ||
		problem.Fault.RetryOwner != protocol.FaultRetryOwnerEngine {
		t.Fatalf("fault = %+v", problem.Fault)
	}
}

func TestClientStreamIdleTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		if flusher, ok := writer.(http.Flusher); ok {
			flusher.Flush()
		}
		<-request.Context().Done()
	}))
	defer server.Close()
	client := testClient()
	client.IdleTimeout = 10 * time.Millisecond
	stream, err := client.Stream(t.Context(), testRequest(t, server.URL, model.ProtocolOpenAIChat))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); !protocol.IsCode(err, protocol.CodeDeadlineExceeded) {
		t.Fatalf("idle Recv() error = %v", err)
	} else {
		problem := protocol.ProblemOf(err)
		if problem.Fault == nil ||
			problem.Fault.Stage != protocol.FaultStageStreamIdle ||
			problem.Fault.Deadline == nil ||
			problem.Fault.Deadline.Scope != protocol.DeadlineProviderStreamIdle ||
			!problem.Fault.Deadline.Renewable {
			t.Fatalf("idle fault = %+v", problem.Fault)
		}
	}
}

// FuzzManagedStreamRecvCloseInterleaving verifies that interleaving Recv()
// and Close() calls on a managedStream does not panic, double-close, or
// corrupt internal state. This catches the double-release bug where the
// idle timeout path and the observe method both call Close().
func FuzzManagedStreamRecvCloseInterleaving(f *testing.F) {
	// Seed corpus: different interleaving patterns.
	f.Add(uint8(0), uint8(0))
	f.Add(uint8(1), uint8(0))
	f.Add(uint8(0), uint8(1))
	f.Add(uint8(5), uint8(5))

	f.Fuzz(func(t *testing.T, recvCalls, closeCalls uint8) {
		recvCalls = uint8(int(recvCalls)%10 + 1)
		closeCalls = uint8(int(closeCalls)%5 + 1)

		var releaseCount int
		var mu sync.Mutex

		release := func() {
			mu.Lock()
			releaseCount++
			mu.Unlock()
		}

		events := make([]provider.StreamEvent, 100)
		for i := range events {
			events[i] = provider.StreamEvent{
				Type: provider.EventTextDelta,
				Text: "x",
			}
		}

		source := &fuzzStream{events: events}
		stream := &managedStream{
			stream:    source,
			cancel:    func() {},
			release:   release,
			closeOnce: sync.Once{},
		}

		var wg sync.WaitGroup

		// Start Recv goroutines.
		for i := uint8(0); i < recvCalls; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = stream.Recv()
			}()
		}

		// Start Close goroutines after a brief delay.
		time.Sleep(5 * time.Millisecond)
		for i := uint8(0); i < closeCalls; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = stream.Close()
			}()
		}

		wg.Wait()

		mu.Lock()
		count := releaseCount
		mu.Unlock()

		// Release must be called at most once (sync.Once protection).
		if count > 1 {
			t.Errorf("release called %d times, expected at most 1", count)
		}
	})
}

// TestStreamCloseIsSafeMultipleTimes verifies that managedStream.Close() is
// safe to call multiple times and that the release function is called exactly
// once. This catches the double-release bug where the idle timeout path calls
// s.Close() (which calls s.release()) and the observe method also calls
// s.Close().
func TestStreamCloseIsSafeMultipleTimes(t *testing.T) {
	var releaseCount int
	var mu sync.Mutex

	release := func() {
		mu.Lock()
		releaseCount++
		mu.Unlock()
	}

	stream := &managedStream{
		stream:    &mockStream{},
		release:   release,
		cancel:    func() {},
		closeOnce: sync.Once{},
	}

	// Call Close multiple times concurrently.
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = stream.Close()
		}()
	}
	wg.Wait()

	mu.Lock()
	count := releaseCount
	mu.Unlock()

	if count != 1 {
		t.Errorf("BUG: release called %d times, expected exactly 1 (double-release detected)", count)
	}
}

func TestIdleTimeoutClosesStreamBeforeReturning(t *testing.T) {
	source := &closeUnblocksStream{closed: make(chan struct{})}
	var released atomic.Int32
	stream := &managedStream{
		stream: source, cancel: func() {},
		release:     func() { released.Add(1) },
		failure:     func(error) {},
		success:     func() {},
		idleTimeout: 10 * time.Millisecond,
	}
	result := make(chan error, 1)
	go func() {
		_, err := stream.Recv()
		result <- err
	}()

	select {
	case err := <-result:
		if !protocol.IsCode(err, protocol.CodeDeadlineExceeded) {
			t.Fatalf("Recv() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Recv() remained blocked after idle timeout")
	}
	if released.Load() != 1 {
		t.Fatalf("release count = %d", released.Load())
	}
}

// fuzzStream is a mock provider.Stream for fuzz testing managedStream.
type fuzzStream struct {
	mu       sync.Mutex
	events   []provider.StreamEvent
	position int
	closed   bool
}

func (s *fuzzStream) Recv() (provider.StreamEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return provider.StreamEvent{}, io.EOF
	}
	if s.position >= len(s.events) {
		return provider.StreamEvent{}, io.EOF
	}
	event := s.events[s.position]
	s.position++
	return event, nil
}

func (s *fuzzStream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

// mockStream implements provider.Stream for testing managedStream.Close safety.
type mockStream struct{}

func (m *mockStream) Recv() (provider.StreamEvent, error) {
	return provider.StreamEvent{}, io.EOF
}

func (m *mockStream) Close() error {
	return nil
}

type closeUnblocksStream struct {
	closed chan struct{}
	once   sync.Once
}

func (s *closeUnblocksStream) Recv() (provider.StreamEvent, error) {
	<-s.closed
	return provider.StreamEvent{}, io.EOF
}

func (s *closeUnblocksStream) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}
