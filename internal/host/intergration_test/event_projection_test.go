package intergration_test

import (
	"fmt"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type eventUpdate interface{ traits() protocol.EventTraits }
type eventUpdateBase struct {
	EventKind   protocol.EventKind
	EventTraits protocol.EventTraits
}

func (u eventUpdateBase) traits() protocol.EventTraits { return u.EventTraits }

type textUpdate struct {
	eventUpdateBase
	Channel, Text string
}
type toolUpdate struct {
	eventUpdateBase
	CallID, Tool, Text string
	Arguments          any
	Result             *protocol.ToolResultData
	State              *protocol.ToolStateData
}
type interactionUpdate struct {
	eventUpdateBase
	ApprovalRequired               *protocol.ApprovalRequiredData
	Source                         *protocol.ApprovalSource
	InputRequired                  *protocol.InputRequiredData
	ResolvedRequest, ResolvedValue string
}
type accountingUpdate struct {
	eventUpdateBase
	Usage   *protocol.UsageData
	Attempt *protocol.ProviderAttemptData
}
type evidenceUpdate struct {
	eventUpdateBase
	Diagnostics  *protocol.DiagnosticsData
	Receipt      *protocol.ExecutionReceiptData
	Verification *protocol.TurnVerificationData
}
type lifecycleUpdate struct {
	eventUpdateBase
	MCPHealth       *protocol.MCPHealthChangedData
	ThreadCompacted *protocol.ThreadCompactedData
	TurnCompaction  *protocol.TurnCompactionData
	TurnReverted    *protocol.TurnRevertedData
}
type artifactUpdate struct {
	eventUpdateBase
	Plan *protocol.PlanDeltaData
}
type agentUpdate struct {
	eventUpdateBase
	Spawned     *protocol.AgentSpawnedData
	Status      *protocol.AgentStatusData
	Message     *protocol.AgentMessageData
	Integration *protocol.AgentIntegrationData
}
type terminalUpdate struct {
	eventUpdateBase
	Status, Message string
	Code            protocol.ErrorCode
	Convergence     *protocol.TurnConvergence
}

type ignoredUpdate struct{ eventUpdateBase }
type unknownUpdate struct {
	eventUpdateBase
	Kind protocol.EventKind
	Raw  []byte
}

// projectEvent interprets protocol events for Benchmark observations.
func projectEvent(event protocol.Event) (eventUpdate, error) {
	if data, ok := event.Data.(*protocol.UnknownEventData); ok {
		return unknownUpdate{
			eventUpdateBase: eventUpdateBase{EventKind: event.Kind},
			Kind:            data.Kind,
			Raw:             append([]byte(nil), data.Raw...),
		}, nil
	}
	traits, ok := protocol.Traits(event.Kind)
	if !ok {
		return nil, fmt.Errorf("event %q has no protocol traits", event.Kind)
	}
	base := eventUpdateBase{EventKind: event.Kind, EventTraits: traits}
	switch data := event.Data.(type) {
	case *protocol.OutputDeltaData:
		return textUpdate{eventUpdateBase: base, Channel: "output", Text: data.Text}, nil
	case *protocol.CommentaryCompletedData:
		return textUpdate{eventUpdateBase: base, Channel: "commentary", Text: data.Text}, nil
	case *protocol.SessionTitleUpdatedData:
		return textUpdate{eventUpdateBase: base, Channel: "session_title", Text: data.Title}, nil
	case *protocol.ReasoningDeltaData:
		return textUpdate{eventUpdateBase: base, Channel: "reasoning", Text: data.Text}, nil
	case *protocol.ReasoningCompletedData:
		return textUpdate{eventUpdateBase: base, Channel: "reasoning", Text: data.Text}, nil
	case *protocol.ToolStartData:
		return toolUpdate{eventUpdateBase: base, CallID: data.CallID, Tool: data.Tool, Arguments: data.Arguments}, nil
	case *protocol.ToolOutputData:
		return toolUpdate{eventUpdateBase: base, CallID: data.CallID, Tool: data.Tool, Text: data.Chunk}, nil
	case *protocol.ToolResultData:
		return toolUpdate{eventUpdateBase: base, CallID: data.CallID, Tool: data.Tool, Text: data.Output, Result: data}, nil
	case *protocol.ToolStateData:
		return toolUpdate{eventUpdateBase: base, State: data}, nil
	case *protocol.ApprovalRequiredData:
		return interactionUpdate{eventUpdateBase: base, ApprovalRequired: data}, nil
	case *protocol.ApprovalResolvedData:
		return interactionUpdate{
			eventUpdateBase: base, Source: data.Source,
			ResolvedRequest: data.RequestID, ResolvedValue: string(data.Decision),
		}, nil
	case *protocol.InputRequiredData:
		return interactionUpdate{eventUpdateBase: base, InputRequired: data}, nil
	case *protocol.InputResolvedData:
		return interactionUpdate{eventUpdateBase: base, ResolvedRequest: data.RequestID, ResolvedValue: data.Answer}, nil
	case *protocol.UsageData:
		return accountingUpdate{eventUpdateBase: base, Usage: data}, nil
	case *protocol.ProviderAttemptData:
		return accountingUpdate{eventUpdateBase: base, Attempt: data}, nil
	case *protocol.DiagnosticsData:
		return evidenceUpdate{eventUpdateBase: base, Diagnostics: data}, nil
	case *protocol.ExecutionReceiptData:
		return evidenceUpdate{eventUpdateBase: base, Receipt: data}, nil
	case *protocol.TurnVerificationData:
		return evidenceUpdate{eventUpdateBase: base, Verification: data}, nil
	case *protocol.MCPHealthChangedData:
		return lifecycleUpdate{eventUpdateBase: base, MCPHealth: data}, nil
	case *protocol.ThreadCompactedData:
		return lifecycleUpdate{eventUpdateBase: base, ThreadCompacted: data}, nil
	case *protocol.TurnCompactionData:
		return lifecycleUpdate{eventUpdateBase: base, TurnCompaction: data}, nil
	case *protocol.TurnRevertedData:
		return lifecycleUpdate{eventUpdateBase: base, TurnReverted: data}, nil
	case *protocol.PlanDeltaData:
		return artifactUpdate{eventUpdateBase: base, Plan: data}, nil
	case *protocol.AgentSpawnedData:
		return agentUpdate{eventUpdateBase: base, Spawned: data}, nil
	case *protocol.AgentStatusData:
		return agentUpdate{eventUpdateBase: base, Status: data}, nil
	case *protocol.AgentMessageData:
		return agentUpdate{eventUpdateBase: base, Message: data}, nil
	case *protocol.AgentIntegrationData:
		return agentUpdate{eventUpdateBase: base, Integration: data}, nil
	case *protocol.TurnCompletedData:
		return terminalUpdate{eventUpdateBase: base, Status: "completed", Message: data.Text}, nil
	case *protocol.TurnFailedData:
		status := "failed"
		if data.Convergence != nil {
			status = "incomplete"
		}
		return terminalUpdate{
			eventUpdateBase: base, Status: status, Code: data.Code,
			Message: data.Message, Convergence: data.Convergence,
		}, nil
	case *protocol.TurnCanceledData:
		return terminalUpdate{eventUpdateBase: base, Status: "canceled", Code: protocol.CodeCanceled, Message: data.Reason}, nil
	case *protocol.OperationRejectedData:
		return terminalUpdate{eventUpdateBase: base, Status: "rejected", Code: data.Code, Message: data.Message}, nil
	default:
		return ignoredUpdate{eventUpdateBase: base}, nil
	}
}
