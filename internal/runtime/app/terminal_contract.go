package app

import (
	"encoding/json"

	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type TerminalMaterial struct {
	FrozenState  turnkernel.State
	DomainFacts  []turnkernel.DomainFact
	Measurement  turnkernel.TerminalMeasurementSnapshot
	Receipt      *protocol.ExecutionReceiptData
	Terminal     protocol.EventData
	SessionDelta json.RawMessage
}

type TerminalRequest struct {
	Operation protocol.Operation
	Material  TerminalMaterial
}

type CommittedTerminal struct {
	Operation          protocol.Operation
	OperationCommitted bool
	OperationID        protocol.OperationID
	ItemID             protocol.ItemID
}
