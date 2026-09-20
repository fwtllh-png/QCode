package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// operationPayloads drives decoding and advertised kinds in contract order.
var operationPayloads = []struct {
	kind       OperationKind
	newPayload func() OperationPayload
}{
	{OperationStartTurn, func() OperationPayload { return &StartTurnPayload{} }},
	{OperationCancelTurn, func() OperationPayload { return &CancelTurnPayload{} }},
	{OperationSteerTurn, func() OperationPayload { return &SteerTurnPayload{} }},
	{OperationEnqueueTurn, func() OperationPayload { return &EnqueueTurnPayload{} }},
	{OperationUpdateQueuedTurn, func() OperationPayload { return &UpdateQueuedTurnPayload{} }},
	{OperationRemoveQueuedTurn, func() OperationPayload { return &RemoveQueuedTurnPayload{} }},
	{OperationPromoteQueuedTurn, func() OperationPayload { return &PromoteQueuedTurnPayload{} }},
	{OperationApprovalDecision, func() OperationPayload { return &ApprovalDecisionPayload{} }},
	{OperationInputReply, func() OperationPayload { return &InputReplyPayload{} }},
	{OperationCompactThread, func() OperationPayload { return &CompactThreadPayload{} }},
	{OperationForkThread, func() OperationPayload { return &ForkThreadPayload{} }},
	{OperationRevertTurn, func() OperationPayload { return &RevertTurnPayload{} }},
}

// eventData mirrors operationPayloads for the outbound direction.
var eventData = []struct {
	kind    EventKind
	newData func() EventData
}{
	{EventTurnStarted, func() EventData { return &TurnStartedData{} }},
	{EventOutputDelta, func() EventData { return &OutputDeltaData{} }},
	{EventOutputDraft, func() EventData { return &OutputDraftData{} }},
	{EventOutputDiscarded, func() EventData { return &OutputDiscardedData{} }},
	{EventCommentaryCompleted, func() EventData { return &CommentaryCompletedData{} }},
	{EventSessionTitleUpdated, func() EventData { return &SessionTitleUpdatedData{} }},
	{EventReasoningDelta, func() EventData { return &ReasoningDeltaData{} }},
	{EventReasoningCompleted, func() EventData { return &ReasoningCompletedData{} }},
	{EventSearchResult, func() EventData { return &SearchResultData{} }},
	{EventCitation, func() EventData { return &CitationData{} }},
	{EventUsage, func() EventData { return &UsageData{} }},
	{EventProviderAttempt, func() EventData { return &ProviderAttemptData{} }},
	{EventToolState, func() EventData { return &ToolStateData{} }},
	{EventToolStart, func() EventData { return &ToolStartData{} }},
	{EventToolOutput, func() EventData { return &ToolOutputData{} }},
	{EventToolResult, func() EventData { return &ToolResultData{} }},
	{EventToolCatalogChanged, func() EventData { return &ToolCatalogChangedData{} }},
	{EventMCPHealthChanged, func() EventData { return &MCPHealthChangedData{} }},
	{EventExtensionControl, func() EventData { return &ExtensionControlData{} }},
	{EventDiagnostics, func() EventData { return &DiagnosticsData{} }},
	{EventTurnCompleted, func() EventData { return &TurnCompletedData{} }},
	{EventTurnFailed, func() EventData { return &TurnFailedData{} }},
	{EventTurnCanceled, func() EventData { return &TurnCanceledData{} }},
	{EventOperationRejected, func() EventData { return &OperationRejectedData{} }},
	{EventTurnSteered, func() EventData { return &TurnSteeredData{} }},
	{EventTurnQueued, func() EventData { return &TurnQueuedData{} }},
	{EventQueuedTurnUpdated, func() EventData { return &QueuedTurnUpdatedData{} }},
	{EventQueuedTurnRemoved, func() EventData { return &QueuedTurnRemovedData{} }},
	{EventApprovalRequired, func() EventData { return &ApprovalRequiredData{} }},
	{EventApprovalResolved, func() EventData { return &ApprovalResolvedData{} }},
	{EventInputRequired, func() EventData { return &InputRequiredData{} }},
	{EventInputResolved, func() EventData { return &InputResolvedData{} }},
	{EventThreadCompacted, func() EventData { return &ThreadCompactedData{} }},
	{EventThreadForked, func() EventData { return &ThreadForkedData{} }},
	{EventTurnReverted, func() EventData { return &TurnRevertedData{} }},
	{EventCheckpointCreated, func() EventData { return &CheckpointCreatedData{} }},
	{EventCheckpointRestored, func() EventData { return &CheckpointRestoredData{} }},
	{EventTurnWithdrawn, func() EventData { return &TurnWithdrawnData{} }},
	{EventCheckpointForked, func() EventData { return &CheckpointForkedData{} }},
	{EventTurnCompaction, func() EventData { return &TurnCompactionData{} }},
	{EventAgentSpawned, func() EventData { return &AgentSpawnedData{} }},
	{EventAgentStatus, func() EventData { return &AgentStatusData{} }},
	{EventAgentMessage, func() EventData { return &AgentMessageData{} }},
	{EventAgentIntegration, func() EventData { return &AgentIntegrationData{} }},
	{EventPlanDelta, func() EventData { return &PlanDeltaData{} }},
	{EventCommandExecution, func() EventData { return &CommandExecutionData{} }},
	{EventHostCommand, func() EventData { return &HostCommandData{} }},
	{EventExecutionReceipt, func() EventData { return &ExecutionReceiptData{} }},
	{EventTurnVerification, func() EventData { return &TurnVerificationData{} }},
}

var (
	operationPayloadIndex = indexOperationPayloads()
	eventDataIndex        = indexEventData()
)

func (o Operation) MarshalJSON() ([]byte, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	type wire struct {
		Version   int              `json:"version"`
		ID        OperationID      `json:"id"`
		Kind      OperationKind    `json:"kind"`
		CreatedAt time.Time        `json:"created_at"`
		Payload   OperationPayload `json:"payload"`
	}
	return json.Marshal(wire(o))
}

func (o *Operation) UnmarshalJSON(data []byte) error {
	var envelope struct {
		Version   int             `json:"version"`
		ID        OperationID     `json:"id"`
		Kind      OperationKind   `json:"kind"`
		CreatedAt time.Time       `json:"created_at"`
		Payload   json.RawMessage `json:"payload"`
	}
	if err := decodeStrict(data, &envelope); err != nil {
		return fmt.Errorf("decode operation envelope: %w", err)
	}
	if envelope.Version != Version {
		return fmt.Errorf("unsupported operation version %d", envelope.Version)
	}
	payload, err := operationPayloadFor(envelope.Kind)
	if err != nil {
		return err
	}
	if len(envelope.Payload) == 0 || bytes.Equal(envelope.Payload, []byte("null")) {
		return errors.New("operation payload is required")
	}
	if err := decodeStrict(envelope.Payload, payload); err != nil {
		return fmt.Errorf("decode %s payload: %w", envelope.Kind, err)
	}
	*o = Operation{
		Version: envelope.Version, ID: envelope.ID, Kind: envelope.Kind,
		CreatedAt: envelope.CreatedAt, Payload: payload,
	}
	return o.Validate()
}

func (e Event) MarshalJSON() ([]byte, error) {
	if err := e.Validate(); err != nil {
		return nil, err
	}
	type wire struct {
		Version     int         `json:"version"`
		ID          EventID     `json:"id"`
		Sequence    Cursor      `json:"sequence"`
		OperationID OperationID `json:"operation_id"`
		ThreadID    ThreadID    `json:"thread_id"`
		TurnID      TurnID      `json:"turn_id"`
		ItemID      ItemID      `json:"item_id"`
		Kind        EventKind   `json:"kind"`
		CreatedAt   time.Time   `json:"created_at"`
		Data        EventData   `json:"data"`
	}
	return json.Marshal(wire(e))
}

func (e *Event) UnmarshalJSON(data []byte) error {
	var envelope struct {
		Version     int             `json:"version"`
		ID          EventID         `json:"id"`
		Sequence    Cursor          `json:"sequence"`
		OperationID OperationID     `json:"operation_id"`
		ThreadID    ThreadID        `json:"thread_id"`
		TurnID      TurnID          `json:"turn_id"`
		ItemID      ItemID          `json:"item_id"`
		Kind        EventKind       `json:"kind"`
		CreatedAt   time.Time       `json:"created_at"`
		Data        json.RawMessage `json:"data"`
	}
	if err := decodeStrict(data, &envelope); err != nil {
		return fmt.Errorf("decode event envelope: %w", err)
	}
	if envelope.Version != Version {
		return fmt.Errorf("unsupported event version %d", envelope.Version)
	}
	if len(envelope.Data) == 0 || bytes.Equal(envelope.Data, []byte("null")) {
		return errors.New("event data is required")
	}
	eventData, err := eventDataFor(envelope.Kind)
	if err != nil {
		eventData = &UnknownEventData{
			Kind: envelope.Kind,
			Raw:  append(json.RawMessage(nil), envelope.Data...),
		}
	} else if err := decodeStrict(envelope.Data, eventData); err != nil {
		return fmt.Errorf("decode %s data: %w", envelope.Kind, err)
	}
	*e = Event{
		Version: envelope.Version, ID: envelope.ID, Sequence: envelope.Sequence,
		OperationID: envelope.OperationID, ThreadID: envelope.ThreadID,
		TurnID: envelope.TurnID, ItemID: envelope.ItemID, Kind: envelope.Kind,
		CreatedAt: envelope.CreatedAt, Data: eventData,
	}
	return e.Validate()
}

func indexOperationPayloads() map[OperationKind]func() OperationPayload {
	index := make(map[OperationKind]func() OperationPayload, len(operationPayloads))
	for _, entry := range operationPayloads {
		index[entry.kind] = entry.newPayload
	}
	return index
}

func indexEventData() map[EventKind]func() EventData {
	index := make(map[EventKind]func() EventData, len(eventData))
	for _, entry := range eventData {
		index[entry.kind] = entry.newData
	}
	return index
}

// OperationKinds returns the operation kinds this build accepts, in the stable
// order hosts advertise during capability negotiation.
func OperationKinds() []OperationKind {
	kinds := make([]OperationKind, 0, len(operationPayloads))
	for _, entry := range operationPayloads {
		kinds = append(kinds, entry.kind)
	}
	return kinds
}

// EventKinds returns the event kinds this build emits, in the stable order hosts
// advertise during capability negotiation.
func EventKinds() []EventKind {
	kinds := make([]EventKind, 0, len(eventData))
	for _, entry := range eventData {
		kinds = append(kinds, entry.kind)
	}
	return kinds
}

func operationPayloadFor(kind OperationKind) (OperationPayload, error) {
	newPayload, known := operationPayloadIndex[kind]
	if !known {
		return nil, fmt.Errorf("unknown operation kind %q", kind)
	}
	return newPayload(), nil
}

func eventDataFor(kind EventKind) (EventData, error) {
	newData, known := eventDataIndex[kind]
	if !known {
		return nil, fmt.Errorf("unknown event kind %q", kind)
	}
	return newData(), nil
}

// DecodeOperationPayload decodes a payload strictly but does not validate it, so
// a host can fill the thread, turn, and item references a thin client is not
// expected to mint before NewOperation validates the finished operation.
func DecodeOperationPayload(kind OperationKind, data json.RawMessage) (OperationPayload, error) {
	payload, err := operationPayloadFor(kind)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		return nil, errors.New("operation payload is required")
	}
	if err := decodeStrict(data, payload); err != nil {
		return nil, fmt.Errorf("decode %s payload: %w", kind, err)
	}
	return payload, nil
}

func decodeStrict(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}
