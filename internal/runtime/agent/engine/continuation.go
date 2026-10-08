package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
)

func currentTurnMessages(
	transaction []provider.Message,
	turn uint64,
) []provider.Message {
	var current []provider.Message
	for _, message := range transaction {
		if message.Turn == turn {
			current = append(current, message)
		}
	}
	return current
}

// loadTurnContinuation restores the durable conversation of a restarted
// Turn. Recovery order is fixed by the caller: the kernel's execution facts
// are already restored when this runs, so this step only rehydrates the
// conversation those facts accepted, and refuses records whose environment
// identity no longer matches the Turn spec.
func (e *Engine) loadTurnContinuation(
	ctx context.Context,
	kernel *turnkernel.RuntimeKernel,
	spec TurnSpec,
) (agentcontext.TurnContinuation, bool, error) {
	blobs := e.options.TurnContinuations
	if blobs == nil {
		return agentcontext.TurnContinuation{}, false, nil
	}
	cursor, ok := kernel.ContinuationCursor()
	if !ok {
		return agentcontext.TurnContinuation{}, false, nil
	}
	record, err := agentcontext.LoadTurnContinuation(
		ctx,
		blobs,
		cursor.Handle,
		cursor.Digest,
	)
	if err != nil {
		return agentcontext.TurnContinuation{}, false, err
	}
	if drift := record.EnvironmentDrift(
		agentcontext.ContinuationEnvironment{
			WorkspaceIdentity: e.options.WorkspaceIdentity,
			ProfileRevision:   spec.Identity.ProfileRevision,
			Provider:          spec.Provider,
			Model:             spec.Model,
		},
	); len(drift) != 0 {
		return agentcontext.TurnContinuation{}, false, fmt.Errorf(
			"continuation environment drift: %s",
			strings.Join(drift, "; "),
		)
	}
	if record.TurnID != spec.Identity.TurnID {
		return agentcontext.TurnContinuation{}, false, fmt.Errorf(
			"continuation turn identity %q does not match %q",
			record.TurnID,
			spec.Identity.TurnID,
		)
	}
	if record.ContextCaptured && (record.StateEpoch != max(uint64(1), e.stateEpoch) || record.SessionRevision != e.sessionRevision) {
		return agentcontext.TurnContinuation{}, false, fmt.Errorf("continuation context revision or epoch changed")
	}
	// The record's turn number is the saving engine's local ordering; the
	// restoring engine owns the authoritative numbering for this session.
	for index := range record.Messages {
		record.Messages[index].Turn = e.turn
	}
	return record, true, nil
}

// continuationEnvironmentComplete reports whether the running Turn carries
// the identity a durable continuation needs; without it a stored record
// could never be drift-checked on restore.
func (e *Engine) continuationEnvironmentComplete(spec TurnSpec) bool {
	return strings.TrimSpace(e.options.WorkspaceIdentity) != "" &&
		spec.Identity.ProfileRevision != 0 &&
		strings.TrimSpace(spec.Provider) != "" &&
		strings.TrimSpace(spec.Model) != ""
}
