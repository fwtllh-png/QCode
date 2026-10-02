package app

import (
	"context"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type EngineSink interface {
	Emit(protocol.EventData) error
}

type TerminalCommitSink interface {
	CommitTerminal(TerminalMaterial) error
}

type Engine interface {
	StartTurn(context.Context, *protocol.StartTurnPayload, EngineSink) error
	CancelTurn(context.Context, *protocol.CancelTurnPayload, EngineSink) error
	SteerTurn(context.Context, *protocol.SteerTurnPayload, EngineSink) error
	DecideApproval(
		context.Context,
		*protocol.ApprovalDecisionPayload,
		EngineSink,
	) error
	ReplyInput(context.Context, *protocol.InputReplyPayload, EngineSink) error
	CompactThread(
		context.Context,
		*protocol.CompactThreadPayload,
		EngineSink,
	) error
	ForkThread(context.Context, *protocol.ForkThreadPayload, EngineSink) error
	RevertTurn(context.Context, *protocol.RevertTurnPayload, EngineSink) error
}

type PendingApproval struct {
	RequestID string
	ThreadID  protocol.ThreadID
	TurnID    protocol.TurnID
	ItemID    protocol.ItemID
	Data      protocol.ApprovalRequiredData
}

type PendingInput struct {
	RequestID string
	ThreadID  protocol.ThreadID
	TurnID    protocol.TurnID
	ItemID    protocol.ItemID
	Data      protocol.InputRequiredData
}
