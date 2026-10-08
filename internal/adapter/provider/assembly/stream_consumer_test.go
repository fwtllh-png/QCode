package assembly

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	providerfixture "github.com/fwtllh-png/QCode/internal/adapter/provider/fixture"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestConsumeStreamStopsDuplicateMembersBeforeNextProviderOutput(t *testing.T) {
	source := newControlledDeltaStream()
	defer source.Close()
	assembly := NewResponseAssembly("degenerate")
	var checkpoint *ResponseAssembly
	done := make(chan error, 1)
	go func() {
		result, err := ConsumeStream(source, assembly, ConsumeConfig{
			Checkpoint: func(value *ResponseAssembly) error {
				checkpoint = CloneResponseAssembly(value)
				return nil
			},
		})
		if len(result.Calls) != 0 {
			done <- errors.New("malformed tool call became executable")
			return
		}
		done <- err
	}()
	for _, fragment := range []string{`{"changes":[],"dry_run":false,`, `"dry_`, `run"`} {
		request := source.nextRequest(t)
		request <- streamResult{event: StreamEvent{
			Type:     EventToolCallDelta,
			ToolCall: &ToolCallFragment{ID: "call", Name: "file_apply", Arguments: fragment},
		}}
	}
	// The provider supplies neither another token nor a stop event. Detection
	// must close its blocked read rather than depend on EOF or idle timeout.
	select {
	case err := <-done:
		var failure *provider.Failure
		if !errors.As(err, &failure) || failure.Code != provider.FailureMalformedResponse ||
			!strings.Contains(err.Error(), "duplicate JSON object member") {
			t.Fatalf("error = %v", err)
		}
		if problem := protocol.ProblemOf(err); problem == nil || problem.Retryable {
			t.Fatalf("malformed output was scheduled for automatic retry: %+v", problem)
		}
		var incomplete *IncompleteOutputError
		if errors.As(err, &incomplete) {
			t.Fatal("duplicate members were classified as continuable output")
		}
	case <-time.After(time.Second):
		t.Fatal("duplicate member detection waited for more provider output")
	}
	if checkpoint == nil || checkpoint.State != ResponseFailed || assembly.State != ResponseFailed {
		t.Fatalf("failed response was not checkpointed: %+v", checkpoint)
	}
	if err := checkpoint.Validate(); err != nil {
		t.Fatalf("failed checkpoint is invalid: %v", err)
	}
	select {
	case <-source.closed:
	default:
		t.Fatal("provider stream was not closed")
	}
}

func TestConsumeStreamMemberTrackingIgnoresReplaysAndSeparatesCalls(t *testing.T) {
	first := StreamEvent{Type: EventToolCallDelta, Sequenced: true, Sequence: 1,
		ToolCall: &ToolCallFragment{Index: 0, ID: "call-0", Name: "read", Arguments: `{"path":"a",`}}
	source := &providerfixture.SliceStream{Events: []StreamEvent{
		first, first,
		{Type: EventToolCallDelta, Sequenced: true, Sequence: 2,
			ToolCall: &ToolCallFragment{Index: 1, ID: "call-1", Name: "read", Arguments: `{"path":`}},
		{Type: EventToolCallDelta, Sequenced: true, Sequence: 3,
			ToolCall: &ToolCallFragment{Index: 0, Arguments: `"options":{"path":"c"}}`}},
		{Type: EventToolCallDelta, Sequenced: true, Sequence: 4,
			ToolCall: &ToolCallFragment{Index: 1, Arguments: `"b"}`}},
		{Type: EventMessageStop, StopReason: StopReasonToolUse},
	}}
	result, err := ConsumeStream(source, NewResponseAssembly("replayed"), ConsumeConfig{})
	if err != nil || len(result.Calls) != 2 || result.Calls[0].Arguments != `{"path":"a","options":{"path":"c"}}` {
		t.Fatalf("result = %+v, error = %v", result, err)
	}
}

func TestConsumeStreamMemberTrackingStartsFreshForContinuation(t *testing.T) {
	assembly := NewResponseAssembly("continued")
	first := &providerfixture.SliceStream{Events: []StreamEvent{
		{Type: EventToolCallDelta, ToolCall: &ToolCallFragment{ID: "call", Name: "read", Arguments: `{"path":"a",`}},
		{Type: EventMessageStop, StopReason: StopReasonMaxTokens},
	}}
	if _, err := ConsumeStream(first, assembly, ConsumeConfig{}); err == nil {
		t.Fatal("unfinished call was accepted")
	}
	second := &providerfixture.SliceStream{Events: []StreamEvent{
		{Type: EventToolCallDelta, ToolCall: &ToolCallFragment{ID: "call", Name: "read", Arguments: `{"path":"a"}`}},
		{Type: EventMessageStop, StopReason: StopReasonToolUse},
	}}
	result, err := ConsumeStream(second, assembly, ConsumeConfig{})
	if err != nil || len(result.Calls) != 1 || len(assembly.Segments) != 2 {
		t.Fatalf("result = %+v, error = %v", result, err)
	}
}
