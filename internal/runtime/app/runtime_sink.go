package app

import (
	"context"
	"errors"

	"github.com/fwtllh-png/QCode/internal/runtime/app/eventhub"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type runtimeSink struct {
	runtime                 *Runtime
	operation               protocol.Operation
	deferTerminal           bool
	terminal                protocol.EventData
	committed               *CommittedTerminal
	terminalCommitAttempted bool
}

func (s *runtimeSink) Emit(data protocol.EventData) error {
	switch payload := s.operation.Payload.(type) {
	case *protocol.StartTurnPayload:
		if started, ok := data.(*protocol.TurnStartedData); ok &&
			started.QueueID == "" {
			started.QueueID = payload.QueueID
		}
	case *protocol.SteerTurnPayload:
		if steered, ok := data.(*protocol.TurnSteeredData); ok &&
			steered.QueueID == "" {
			steered.QueueID = payload.QueueID
		}
	case *protocol.PromoteQueuedTurnPayload:
		if steered, ok := data.(*protocol.TurnSteeredData); ok &&
			steered.QueueID == "" {
			steered.QueueID = payload.QueueID
		}
	}
	if s.deferTerminal && protocol.IsTerminalEvent(eventhub.EventKind(data)) {
		if s.terminal == nil {
			s.terminal = data
		}
		return nil
	}
	threadID, turnID, itemID := protocol.OperationReferences(s.operation)
	return s.runtime.publish(s.operation.ID, threadID, turnID, itemID, data)
}

func (s *runtimeSink) EmitStable(
	eventID protocol.EventID,
	data protocol.EventData,
) error {
	threadID, turnID, itemID := protocol.OperationReferences(s.operation)
	return s.runtime.publishStable(
		s.operation.ID,
		threadID,
		turnID,
		itemID,
		eventID,
		data,
	)
}

func (s *runtimeSink) CommitTerminal(material TerminalMaterial) error {
	s.terminalCommitAttempted = true
	committed, err := s.runtime.terminal.Commit(
		context.Background(),
		TerminalRequest{Operation: s.operation, Material: material},
	)
	if err != nil {
		return err
	}
	s.committed = &committed
	s.terminal = material.Terminal
	return nil
}

func (s *runtimeSink) publishTerminal() error {
	return s.publishTerminalAs(s.operation.ID, s.operationItemID())
}

func (s *runtimeSink) operationItemID() protocol.ItemID {
	_, _, itemID := protocol.OperationReferences(s.operation)
	return itemID
}

func (s *runtimeSink) commitOperation() {
	if s.committed != nil && s.committed.OperationCommitted {
		s.runtime.commitLocal(s.operation.ID)
		return
	}
	s.runtime.commit(s.operation.ID)
}

func (s *runtimeSink) publishTerminalAs(
	operationID protocol.OperationID,
	itemID protocol.ItemID,
) error {
	if s.terminal == nil {
		return errors.New("turn finished without a terminal event")
	}
	threadID, turnID, _ := protocol.OperationReferences(s.operation)
	if s.committed == nil {
		return s.runtime.publish(
			operationID,
			threadID,
			turnID,
			itemID,
			s.terminal,
		)
	}
	committed := *s.committed
	committed.OperationID, committed.ItemID = operationID, itemID
	if err := s.runtime.terminal.Publish(context.Background(), committed); err != nil {
		return err
	}
	s.publishPostTurnContextMaintenance(
		operationID,
		threadID,
		turnID,
		itemID,
	)
	return nil
}
