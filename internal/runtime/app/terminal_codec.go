package app

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
