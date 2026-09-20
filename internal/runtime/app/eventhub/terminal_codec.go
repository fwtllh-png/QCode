package eventhub

import (
	"encoding/json"
	"fmt"

	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func DecodeTerminalOutboxEntry(
	entry turnkernel.ProjectionOutboxEntry,
) (protocol.EventData, error) {
	var data protocol.EventData
	switch protocol.EventKind(entry.Kind) {
	case protocol.EventOutputDelta:
		data = &protocol.OutputDeltaData{}
	case protocol.EventCommentaryCompleted:
		data = &protocol.CommentaryCompletedData{}
	case protocol.EventExecutionReceipt:
		data = &protocol.ExecutionReceiptData{}
	case protocol.EventTurnCompleted:
		data = &protocol.TurnCompletedData{}
	case protocol.EventTurnFailed:
		data = &protocol.TurnFailedData{}
	case protocol.EventTurnCanceled:
		data = &protocol.TurnCanceledData{}
	default:
		return nil, fmt.Errorf(
			"unsupported terminal outbox kind %q",
			entry.Kind,
		)
	}
	if err := json.Unmarshal(entry.Payload, data); err != nil {
		return nil, fmt.Errorf(
			"decode terminal outbox %s: %w",
			entry.ID,
			err,
		)
	}
	return data, nil
}

func EventKind(data protocol.EventData) protocol.EventKind {
	switch data.(type) {
	case *protocol.TurnStartedData:
		return protocol.EventTurnStarted
	case *protocol.OutputDeltaData:
		return protocol.EventOutputDelta
	case *protocol.OutputDraftData:
		return protocol.EventOutputDraft
	case *protocol.OutputDiscardedData:
		return protocol.EventOutputDiscarded
	case *protocol.CommentaryCompletedData:
		return protocol.EventCommentaryCompleted
	case *protocol.SessionTitleUpdatedData:
		return protocol.EventSessionTitleUpdated
	case *protocol.ReasoningDeltaData:
		return protocol.EventReasoningDelta
	case *protocol.ReasoningCompletedData:
		return protocol.EventReasoningCompleted
	case *protocol.SearchResultData:
		return protocol.EventSearchResult
	case *protocol.CitationData:
		return protocol.EventCitation
	case *protocol.UsageData:
		return protocol.EventUsage
	case *protocol.ToolStateData:
		return protocol.EventToolState
	case *protocol.ToolStartData:
		return protocol.EventToolStart
	case *protocol.ToolOutputData:
		return protocol.EventToolOutput
	case *protocol.ToolResultData:
		return protocol.EventToolResult
	case *protocol.DiagnosticsData:
		return protocol.EventDiagnostics
	case *protocol.TurnCompletedData:
		return protocol.EventTurnCompleted
	case *protocol.TurnFailedData:
		return protocol.EventTurnFailed
	case *protocol.TurnCanceledData:
		return protocol.EventTurnCanceled
	case *protocol.OperationRejectedData:
		return protocol.EventOperationRejected
	case *protocol.TurnSteeredData:
		return protocol.EventTurnSteered
	case *protocol.ApprovalRequiredData:
		return protocol.EventApprovalRequired
	case *protocol.ApprovalResolvedData:
		return protocol.EventApprovalResolved
	case *protocol.InputRequiredData:
		return protocol.EventInputRequired
	case *protocol.InputResolvedData:
		return protocol.EventInputResolved
	case *protocol.ThreadCompactedData:
		return protocol.EventThreadCompacted
	case *protocol.ThreadForkedData:
		return protocol.EventThreadForked
	case *protocol.TurnRevertedData:
		return protocol.EventTurnReverted
	case *protocol.CheckpointCreatedData:
		return protocol.EventCheckpointCreated
	case *protocol.CheckpointRestoredData:
		return protocol.EventCheckpointRestored
	case *protocol.CheckpointForkedData:
		return protocol.EventCheckpointForked
	case *protocol.ExecutionReceiptData:
		return protocol.EventExecutionReceipt
	case *protocol.TurnCompactionData:
		return protocol.EventTurnCompaction
	case *protocol.TurnVerificationData:
		return protocol.EventTurnVerification
	case *protocol.AgentSpawnedData:
		return protocol.EventAgentSpawned
	case *protocol.AgentStatusData:
		return protocol.EventAgentStatus
	case *protocol.AgentMessageData:
		return protocol.EventAgentMessage
	case *protocol.AgentIntegrationData:
		return protocol.EventAgentIntegration
	case *protocol.PlanDeltaData:
		return protocol.EventPlanDelta
	case *protocol.CommandExecutionData:
		return protocol.EventCommandExecution
	case *protocol.HostCommandData:
		return protocol.EventHostCommand
	default:
		return ""
	}
}
