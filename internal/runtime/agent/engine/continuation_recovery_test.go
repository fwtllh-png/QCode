package engine

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
)

type mapBlobStore struct {
	mu   sync.Mutex
	data map[string][]byte
}

func newMapBlobStore() *mapBlobStore {
	return &mapBlobStore{data: map[string][]byte{}}
}

func (s *mapBlobStore) Put(
	_ context.Context,
	handle string,
	data []byte,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[handle] = append([]byte(nil), data...)
	return nil
}

func (s *mapBlobStore) Get(
	_ context.Context,
	handle string,
) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.data[handle]
	if !ok {
		return nil, io.ErrUnexpectedEOF
	}
	return append([]byte(nil), data...), nil
}

// gatedStream blocks the second model sample until the test releases it,
// standing in for a process that dies mid-sample after its first tool batch
// was durably accepted. entered closes once Recv is blocking, which is the
// point after which the interrupted engine writes no further facts.
type gatedStream struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newGatedStream() *gatedStream {
	return &gatedStream{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (s *gatedStream) Recv() (provider.StreamEvent, error) {
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return provider.StreamEvent{}, io.EOF
}

func (*gatedStream) Close() error { return nil }

func waitForContinuationFact(
	t *testing.T,
	store *turnkernel.MemoryTerminalEnvelopeStore,
	turnID string,
) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		facts, err := store.LoadDomainFacts(t.Context(), turnID)
		if err == nil {
			for _, fact := range facts {
				if fact.Command == "continuation_recorded" {
					return
				}
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("continuation fact was not committed before the restart")
}

func restartTestEngines(
	t *testing.T,
	workspaceB string,
	configure ...func(*Engine, *scriptedProvider),
) (*scriptedProvider, *Engine, *echoTool, *turnkernel.MemoryTerminalEnvelopeStore) {
	t.Helper()
	store := turnkernel.NewMemoryTerminalEnvelopeStore(nil, nil)
	coordinators, err := turnkernel.NewStoreCoordinatorRuntime(store)
	if err != nil {
		t.Fatal(err)
	}
	blobs := newMapBlobStore()
	registry := tool.NewRegistry(nil, nil)
	echo := &echoTool{}
	if err := registry.Register(echo); err != nil {
		t.Fatal(err)
	}

	releaseA := newGatedStream()
	providerA := &scriptedProvider{streams: []provider.Stream{
		toolCallStream("call-1", "echo", `{"text":"probe"}`),
		releaseA,
	}}
	engineA := newEngine(t, providerA, registry)
	engineA.options.TurnCoordinatorRuntime = coordinators
	engineA.options.TurnContinuations = blobs
	engineA.options.WorkspaceIdentity = "workspace-restart"
	engineA.options.ProfileRevision = 1
	for _, setup := range configure {
		setup(engineA, providerA)
	}

	doneA := make(chan error, 1)
	go func() {
		_, runErr := engineA.RunForTurn(
			t.Context(),
			"turn-restart",
			"inspect the parser",
			func(Event) error { return nil },
		)
		doneA <- runErr
	}()
	waitForContinuationFact(t, store, "turn-restart")
	// The accepted conversation is durable and the interrupted engine is
	// blocked inside its next sample, so no further facts can race the
	// restoring engine.
	select {
	case <-releaseA.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("interrupted engine never entered its second sample")
	}

	// The coordinator lease expiring stands in for process death while the
	// second sample is still in flight.
	if err := coordinators.Release(t.Context(), "turn-restart"); err != nil {
		t.Fatal(err)
	}

	providerB := &scriptedProvider{streams: []provider.Stream{
		textStream("resumed"),
	}}
	engineB := newEngine(t, providerB, registry)
	engineB.options.TurnCoordinatorRuntime = coordinators
	engineB.options.TurnContinuations = blobs
	engineB.options.WorkspaceIdentity = workspaceB
	engineB.options.ProfileRevision = 1

	t.Cleanup(func() {
		close(releaseA.release)
		<-doneA
	})
	return providerB, engineB, echo, store
}

// A restarted Turn's first provider request must carry the conversation the
// interrupted process already accepted, and the completed tool effect must
// not be dispatched a second time.
func TestRestartedTurnRebuildsAcceptedConversation(t *testing.T) {
	providerB, engineB, echo, _ := restartTestEngines(t, "workspace-restart")

	result, err := engineB.RunForTurn(
		t.Context(),
		"turn-restart",
		"inspect the parser",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != Completed {
		t.Fatalf("restarted result state = %v", result.State)
	}
	if echo.calls.Load() != 1 {
		t.Fatalf(
			"completed tool effect re-dispatched: executions = %d",
			echo.calls.Load(),
		)
	}
	if len(providerB.requests) == 0 {
		t.Fatal("restarted engine sampled nothing")
	}
	request := providerB.requests[0]
	if !requestContains(request, "inspect the parser") ||
		!requestContainsToolResult(request, "probe") ||
		!requestContainsToolCall(request, "call-1") {
		t.Fatalf(
			"restarted first request lost the accepted conversation: %+v",
			request.Messages,
		)
	}
}

// A continuation record from a different execution environment must not seed
// the restored conversation, and the rejection has to be observable.
func TestRestartedTurnRejectsContinuationOnEnvironmentDrift(t *testing.T) {
	providerB, engineB, _, _ := restartTestEngines(t, "workspace-other")

	_, err := engineB.RunForTurn(t.Context(), "turn-restart", "inspect the parser", nil)
	if err == nil || !strings.Contains(err.Error(), "workspace identity changed") {
		t.Fatalf("continuation drift was not rejected: %v", err)
	}
	if len(providerB.requests) != 0 {
		t.Fatal("sampled without the accepted context after environment drift")
	}
}

func requestContainsToolCall(
	request provider.ModelRequest,
	callID string,
) bool {
	for _, message := range request.Messages {
		for _, block := range message.Blocks {
			if block.ToolCall != nil && block.ToolCall.ID == callID {
				return true
			}
		}
	}
	return false
}
