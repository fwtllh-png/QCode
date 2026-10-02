package agentcontext

import (
	"fmt"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// ThreadReconstruction is the pure result of replaying durable events (N11).
type ThreadReconstruction struct {
	History []provider.Message
	Window  reconstructWindow
}

type reconstructWindow struct {
	Number  uint64
	FirstID string
	Current string
}

// ReconstructThread rebuilds model-visible history for threadID from durable events.
// It seeds from the newest compacted/fork ReplacementHistory, then forward-applies
// completed turns, safely paired user-interrupted turns, and later compact
// checkpoints. Failed, incomplete, and non-user cancellation traffic must not
// leak into a resumed provider request.
func ReconstructThread(events []protocol.Event, threadID protocol.ThreadID) (ThreadReconstruction, error) {
	if threadID == "" {
		return ThreadReconstruction{}, fmt.Errorf("thread id is required")
	}
	baseIndex := -1
	var baseHistory []provider.Message
	var window reconstructWindow
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		switch event.Kind {
		case protocol.EventThreadCompacted:
			if event.ThreadID != threadID {
				continue
			}
			data, ok := event.Data.(*protocol.ThreadCompactedData)
			if !ok || data == nil || len(data.ReplacementHistory) == 0 {
				continue
			}
			messages, err := DecodeCompactedHistory(data.ReplacementHistory)
			if err != nil {
				return ThreadReconstruction{}, err
			}
			baseHistory = messages
			baseIndex = i
			window = reconstructWindow{
				Number: data.WindowNumber, FirstID: data.FirstWindowID, Current: data.WindowID,
			}
		case protocol.EventThreadForked:
			data, ok := event.Data.(*protocol.ThreadForkedData)
			if !ok || data == nil || data.NewThreadID != threadID || len(data.ReplacementHistory) == 0 {
				continue
			}
			messages, err := DecodeCompactedHistory(data.ReplacementHistory)
			if err != nil {
				return ThreadReconstruction{}, err
			}
			baseHistory = messages
			baseIndex = i
			windowID := data.WindowID
			if windowID == "" {
				windowID = "fork:" + string(threadID)
			}
			number := data.WindowNumber
			if number == 0 {
				number = 1
			}
			firstID := data.FirstWindowID
			if firstID == "" {
				firstID = windowID
			}
			window = reconstructWindow{
				Number: number, FirstID: firstID, Current: windowID,
			}
		case protocol.EventCheckpointRestored:
			if event.ThreadID != threadID {
				continue
			}
			data, ok := event.Data.(*protocol.CheckpointRestoredData)
			if !ok || data == nil || len(data.ReplacementHistory) == 0 {
				continue
			}
			messages, err := DecodeCompactedHistory(data.ReplacementHistory)
			if err != nil {
				return ThreadReconstruction{}, err
			}
			baseHistory = messages
			baseIndex = i
			windowID := "checkpoint:" + data.CheckpointID
			window = reconstructWindow{Number: 1, FirstID: windowID, Current: windowID}
		case protocol.EventCheckpointForked:
			data, ok := event.Data.(*protocol.CheckpointForkedData)
			if !ok || data == nil || data.NewThreadID != threadID ||
				len(data.ReplacementHistory) == 0 {
				continue
			}
			messages, err := DecodeCompactedHistory(data.ReplacementHistory)
			if err != nil {
				return ThreadReconstruction{}, err
			}
			baseHistory = messages
			baseIndex = i
			windowID := "checkpoint-fork:" + data.CheckpointID
			window = reconstructWindow{Number: 1, FirstID: windowID, Current: windowID}
		default:
			continue
		}
		if baseIndex >= 0 {
			break
		}
	}

	replay := newReplayHistory(baseHistory)
	pending := make(map[protocol.TurnID]*replayTurn)
	start := 0
	if baseIndex >= 0 {
		start = baseIndex + 1
	}
	for _, event := range events[start:] {
		if event.ThreadID != threadID {
			continue
		}
		switch event.Kind {
		case protocol.EventThreadCompacted:
			data, ok := event.Data.(*protocol.ThreadCompactedData)
			if !ok || data == nil || len(data.ReplacementHistory) == 0 {
				continue
			}
			messages, err := DecodeCompactedHistory(data.ReplacementHistory)
			if err != nil {
				return ThreadReconstruction{}, err
			}
			replay = newReplayHistory(messages)
			clear(pending)
			window = reconstructWindow{
				Number: data.WindowNumber, FirstID: data.FirstWindowID, Current: data.WindowID,
			}
		case protocol.EventCheckpointRestored:
			data, ok := event.Data.(*protocol.CheckpointRestoredData)
			if !ok || data == nil || len(data.ReplacementHistory) == 0 {
				continue
			}
			messages, err := DecodeCompactedHistory(data.ReplacementHistory)
			if err != nil {
				return ThreadReconstruction{}, err
			}
			replay = newReplayHistory(messages)
			clear(pending)
			windowID := "checkpoint:" + data.CheckpointID
			window = reconstructWindow{Number: 1, FirstID: windowID, Current: windowID}
		case protocol.EventTurnStarted:
			data, ok := event.Data.(*protocol.TurnStartedData)
			if !ok || data == nil || data.Prompt == "" {
				continue
			}
			turn := newReplayTurn()
			turn.messages = append(turn.messages, provider.TextMessage(provider.RoleUser, data.Prompt))
			pending[event.TurnID] = turn
		case protocol.EventTurnSteered:
			data, ok := event.Data.(*protocol.TurnSteeredData)
			if !ok || data == nil || data.Prompt == "" {
				continue
			}
			turn := turnForReplay(pending, event.TurnID)
			turn.messages = append(
				turn.messages,
				provider.TextMessage(provider.RoleUser, data.Prompt),
			)
		case protocol.EventToolStart:
			data, ok := event.Data.(*protocol.ToolStartData)
			if !ok || data == nil || data.CallID == "" || data.Tool == "" {
				continue
			}
			turn := turnForReplay(pending, event.TurnID)
			turn.starts[data.CallID] = struct{}{}
			turn.messages = append(turn.messages, provider.Message{
				Role: provider.RoleAssistant,
				Blocks: []provider.ContentBlock{{
					Type: provider.ContentToolCall,
					ToolCall: &provider.ToolCall{
						ID: data.CallID, Name: data.Tool, Arguments: string(data.Arguments),
					},
				}},
			})
		case protocol.EventToolResult:
			data, ok := event.Data.(*protocol.ToolResultData)
			if !ok || data == nil || data.CallID == "" {
				continue
			}
			turn := turnForReplay(pending, event.TurnID)
			turn.results[data.CallID] = struct{}{}
			turn.messages = append(turn.messages, provider.Message{
				Role: provider.RoleTool,
				Blocks: []provider.ContentBlock{{
					Type: provider.ContentToolResult,
					ToolResult: &provider.ToolResult{
						CallID: data.CallID, Content: data.Output, IsError: data.IsError,
					},
				}},
			})
		case protocol.EventTurnCompleted:
			data, ok := event.Data.(*protocol.TurnCompletedData)
			if !ok || data == nil {
				continue
			}
			turn := turnForReplay(pending, event.TurnID)
			if data.Text != "" {
				turn.messages = append(
					turn.messages,
					provider.TextMessage(provider.RoleAssistant, data.Text),
				)
			}
			for _, message := range turn.pairedMessages() {
				replay.append(event.TurnID, message)
			}
			delete(pending, event.TurnID)
		case protocol.EventTurnCanceled:
			data, ok := event.Data.(*protocol.TurnCanceledData)
			if ok && data != nil && data.Reason == protocol.CancelReasonUserInterrupted {
				turn := turnForReplay(pending, event.TurnID)
				for _, message := range turn.pairedMessages() {
					replay.append(event.TurnID, message)
				}
			}
			delete(pending, event.TurnID)
		case protocol.EventTurnFailed:
			delete(pending, event.TurnID)
		case protocol.EventTurnReverted:
			data, ok := event.Data.(*protocol.TurnRevertedData)
			if !ok || data == nil {
				continue
			}
			delete(pending, data.TargetTurnID)
			replay.dropTurn(data.TargetTurnID)
		}
	}
	return ThreadReconstruction{History: replay.messages, Window: window}, nil
}

type replayTurn struct {
	messages []provider.Message
	starts   map[string]struct{}
	results  map[string]struct{}
}

func newReplayTurn() *replayTurn {
	return &replayTurn{
		starts:  make(map[string]struct{}),
		results: make(map[string]struct{}),
	}
}

func turnForReplay(
	pending map[protocol.TurnID]*replayTurn,
	turnID protocol.TurnID,
) *replayTurn {
	turn := pending[turnID]
	if turn == nil {
		turn = newReplayTurn()
		pending[turnID] = turn
	}
	return turn
}

func (t *replayTurn) pairedMessages() []provider.Message {
	messages := make([]provider.Message, 0, len(t.messages))
	for _, message := range t.messages {
		if callID := replayToolCallID(message); callID != "" {
			if _, paired := t.results[callID]; !paired {
				continue
			}
		}
		if callID := replayToolResultID(message); callID != "" {
			if _, paired := t.starts[callID]; !paired {
				continue
			}
		}
		messages = append(messages, message)
	}
	return messages
}

func replayToolCallID(message provider.Message) string {
	for _, block := range message.Blocks {
		if block.Type == provider.ContentToolCall && block.ToolCall != nil {
			return block.ToolCall.ID
		}
	}
	return ""
}

func replayToolResultID(message provider.Message) string {
	for _, block := range message.Blocks {
		if block.Type == provider.ContentToolResult && block.ToolResult != nil {
			return block.ToolResult.CallID
		}
	}
	return ""
}

// replayHistory tracks which protocol turn produced each replayed message so
// turn.reverted can drop exactly that turn, mirroring Engine.RevertWorkspace.
// Messages seeded from a compact/fork window predate the replayed turns and
// carry no attribution, so they are never dropped by a revert.
type replayHistory struct {
	messages []provider.Message
	turns    []protocol.TurnID
}

func newReplayHistory(seed []provider.Message) replayHistory {
	return replayHistory{
		messages: append([]provider.Message(nil), seed...),
		turns:    make([]protocol.TurnID, len(seed)),
	}
}

func (r *replayHistory) append(turnID protocol.TurnID, message provider.Message) {
	r.messages = append(r.messages, message)
	r.turns = append(r.turns, turnID)
}

func (r *replayHistory) dropTurn(turnID protocol.TurnID) {
	if turnID == "" {
		return
	}
	messages := r.messages[:0]
	turns := r.turns[:0]
	for index, message := range r.messages {
		if r.turns[index] == turnID {
			continue
		}
		messages = append(messages, message)
		turns = append(turns, r.turns[index])
	}
	r.messages = messages
	r.turns = turns
}
