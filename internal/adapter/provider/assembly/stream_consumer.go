package assembly

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// StreamProjection is the provider-neutral output of consuming one stream
// event. Runtime owners decide how to expose it to their hosts.
type StreamProjection struct {
	Text      string
	Block     *provider.ContentBlock
	Search    *provider.SearchResult
	Citation  *provider.Citation
	Usage     *provider.Usage
	Transport provider.TransportMetadata
}

type ConsumeConfig struct {
	FirstOutput func()
	Project     func(StreamProjection) error
	Checkpoint  func(*ResponseAssembly) error
}

type ConsumeResult struct {
	Blocks     []provider.ContentBlock
	Calls      []provider.ToolCall
	Usage      provider.Usage
	Meaningful bool
	Replay     *provider.ReplayState
}

type IncompleteOutputError struct {
	Reason        provider.StopReason
	ToolFragments []provider.ToolCallFragment
	Cause         error
}

func (e *IncompleteOutputError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf(
			"model output stopped before completion (%s): %v",
			e.Reason,
			e.Cause,
		)
	}
	return fmt.Sprintf("model output stopped before completion (%s)", e.Reason)
}

func (e *IncompleteOutputError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// ConsumeStream owns provider stream validation, incremental assembly and
// checkpointing. It returns only complete tool calls; incomplete fragments are
// retained in IncompleteOutputError for a later continuation attempt.
func ConsumeStream(
	stream provider.Stream,
	assembly *ResponseAssembly,
	options ConsumeConfig,
) (ConsumeResult, error) {
	if assembly == nil {
		return ConsumeResult{}, errors.New("response assembly is nil")
	}
	metadata := provider.Metadata(stream)
	stream = NewDeltaCoalescingStream(stream, options.FirstOutput)
	defer stream.Close()
	if metadata.LogicalRequestID == "" {
		metadata.LogicalRequestID = assembly.LogicalRequestID
	}
	if metadata.Attempt == 0 {
		metadata.Attempt = assembly.NextTransportAttempt()
	}
	if err := assembly.BeginTransport(metadata); err != nil {
		return ConsumeResult{}, err
	}
	var checkpointedUnits, pendingCheckpointUnits uint64
	addPendingCheckpoint := func(units uint64) {
		if ^uint64(0)-pendingCheckpointUnits < units {
			pendingCheckpointUnits = ^uint64(0)
			return
		}
		pendingCheckpointUnits += units
	}
	persist := func(force bool) error {
		if options.Checkpoint == nil {
			return nil
		}
		if !force &&
			pendingCheckpointUnits < max(uint64(1), checkpointedUnits) {
			return nil
		}
		if err := options.Checkpoint(assembly); err != nil {
			return err
		}
		if ^uint64(0)-checkpointedUnits < pendingCheckpointUnits {
			checkpointedUnits = ^uint64(0)
		} else {
			checkpointedUnits += pendingCheckpointUnits
		}
		pendingCheckpointUnits = 0
		return nil
	}
	if err := persist(true); err != nil {
		return ConsumeResult{}, err
	}
	project := func(value StreamProjection) error {
		if options.Project == nil {
			return nil
		}
		return options.Project(value)
	}
	output := func() {
		if options.FirstOutput != nil {
			options.FirstOutput()
		}
	}
	current := func() ConsumeResult {
		return ConsumeResult{
			Blocks:     assembly.CurrentBlocks(),
			Usage:      assembly.CurrentUsage(),
			Meaningful: assembly.CurrentMeaningful(),
		}
	}
	argumentMembers := make(map[int]*jsonMemberTracker)
	for {
		event, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = protocol.NewProblem(
					protocol.CodeUnavailable,
					"model stream ended without a valid stop event",
					true,
					io.ErrUnexpectedEOF,
				)
			}
			result := current()
			if interruptErr := assembly.Interrupt(err); interruptErr != nil {
				return result, errors.Join(err, interruptErr)
			}
			if persistErr := persist(true); persistErr != nil {
				return result, persistErr
			}
			if errors.Is(err, context.Canceled) {
				return result, err
			}
			if result.Meaningful {
				return result, &IncompleteOutputError{
					Reason:        provider.StopReasonIncomplete,
					ToolFragments: assembly.IncompleteToolFragments(),
					Cause:         err,
				}
			}
			return result, err
		}
		applied, applyErr := assembly.Apply(event)
		if applyErr == nil && applied && event.Type == provider.EventToolCallDelta {
			fragment := event.ToolCall
			members := argumentMembers[fragment.Index]
			if members == nil {
				members = &jsonMemberTracker{}
				argumentMembers[fragment.Index] = members
			}
			if err := members.Append(fragment.Arguments); err != nil {
				applyErr = fmt.Errorf("tool call %d: %w", fragment.Index, err)
			}
		}
		if applyErr != nil {
			failure := &provider.Failure{
				Code:    provider.FailureMalformedResponse,
				Message: "provider stream violated the incremental response contract",
			}
			if errors.Is(applyErr, errDuplicateJSONMember) {
				failure.Message = "provider generated tool arguments with a duplicate JSON object member"
			}
			_ = assembly.Fail(applyErr)
			if persistErr := persist(true); persistErr != nil {
				return current(), errors.Join(applyErr, persistErr)
			}
			return current(), protocol.NewProblem(
				protocol.CodeUnavailable,
				failure.Message,
				false,
				errors.Join(failure, applyErr),
			)
		}
		if !applied {
			continue
		}
		batchedCheckpoint := coalescibleDelta(event) ||
			event.Type == provider.EventTransportProgress
		if coalescibleDelta(event) {
			addPendingCheckpoint(uint64(max(1, deltaSize(event))))
		} else if batchedCheckpoint {
			addPendingCheckpoint(1)
		}
		if err := persist(!batchedCheckpoint); err != nil {
			return current(), err
		}
		result := current()
		switch event.Type {
		case provider.EventMessageStart, provider.EventTransportProgress:
		case provider.EventTextDelta:
			output()
			block := eventContentBlock(event, provider.ContentText)
			block.Text = event.Text
			if err := project(StreamProjection{
				Text:  event.Text,
				Block: &block,
			}); err != nil {
				return result, err
			}
		case provider.EventReasoningDelta:
			output()
			block := eventContentBlock(event, provider.ContentReasoning)
			block.Text = event.Text
			if block.Text == "" {
				continue
			}
			if err := project(StreamProjection{
				Text:  event.Text,
				Block: &block,
			}); err != nil {
				return result, err
			}
		case provider.EventSearchResult, provider.EventCitation:
			output()
			block := eventContentBlock(event, "")
			if err := project(StreamProjection{
				Block:    &block,
				Search:   event.Search,
				Citation: event.Citation,
			}); err != nil {
				return result, err
			}
		case provider.EventUsage:
			usage := result.Usage
			if err := project(StreamProjection{
				Usage:     &usage,
				Transport: event.Usage.Transport,
			}); err != nil {
				return result, err
			}
		case provider.EventReplayState, provider.EventResponseState:
		case provider.EventToolCallDelta:
			output()
		case provider.EventMessageStop:
			result.Replay = assembly.CurrentReplay()
			reason := assembly.CurrentStopReason()
			switch reason {
			case provider.StopReasonMaxTokens, provider.StopReasonIncomplete:
				return result, &IncompleteOutputError{
					Reason:        reason,
					ToolFragments: assembly.IncompleteToolFragments(),
				}
			case provider.StopReasonContentFilter:
				return result, protocol.NewProblem(
					protocol.CodeInvalidArgument,
					"model output was blocked by the provider content filter",
					false,
					nil,
				)
			case provider.StopReasonUnknown:
				return result, protocol.NewProblem(
					protocol.CodeUnavailable,
					"provider returned an unknown model stop reason",
					true,
					nil,
				)
			}
			calls, callErr := assembly.ExecutableToolCalls()
			if callErr != nil {
				if len(assembly.Segments[len(assembly.Segments)-1].ToolFragments) != 0 {
					return result, &IncompleteOutputError{
						Reason:        provider.StopReasonIncomplete,
						ToolFragments: assembly.IncompleteToolFragments(),
						Cause:         callErr,
					}
				}
				calls = nil
			}
			if reason == provider.StopReasonToolUse && len(calls) == 0 {
				return result, protocol.NewProblem(
					protocol.CodeUnavailable,
					"provider stopped for tool use without emitting a tool call",
					true,
					nil,
				)
			}
			result.Calls = calls
			return result, nil
		default:
			return result, errors.New("unknown provider event")
		}
	}
}

func eventContentBlock(
	event provider.StreamEvent,
	fallback provider.ContentType,
) provider.ContentBlock {
	if event.Block != nil {
		return cloneContentBlock(*event.Block)
	}
	switch event.Type {
	case provider.EventTextDelta:
		return provider.ContentBlock{Type: provider.ContentText, Text: event.Text}
	case provider.EventReasoningDelta:
		return provider.ContentBlock{Type: provider.ContentReasoning, Text: event.Text}
	case provider.EventSearchResult:
		return provider.ContentBlock{Type: provider.ContentSearch, Search: event.Search}
	case provider.EventCitation:
		return provider.ContentBlock{Type: provider.ContentCitation, Citation: event.Citation}
	default:
		return provider.ContentBlock{Type: fallback, Text: event.Text}
	}
}

func cloneContentBlock(block provider.ContentBlock) provider.ContentBlock {
	cloned := block
	if block.ToolCall != nil {
		value := *block.ToolCall
		cloned.ToolCall = &value
	}
	if block.ToolResult != nil {
		value := *block.ToolResult
		cloned.ToolResult = &value
	}
	if block.Search != nil {
		value := *block.Search
		cloned.Search = &value
	}
	if block.Citation != nil {
		value := *block.Citation
		cloned.Citation = &value
	}
	return cloned
}
