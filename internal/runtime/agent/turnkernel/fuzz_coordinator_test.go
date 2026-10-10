package turnkernel

import (
	"fmt"
	"testing"
)

// FuzzCoordinatorEffectDispatchDoesNotRecurseUnboundedly verifies that the
// coordinator's Submit → Dispatch → Submit chain does not cause unbounded
// recursion. This catches the bug where deeply chained effects could overflow
// the stack.
func FuzzCoordinatorEffectDispatchDoesNotRecurseUnboundedly(f *testing.F) {
	f.Add(uint8(1))
	f.Add(uint8(5))
	f.Add(uint8(20))
	f.Add(uint8(100))

	f.Fuzz(func(t *testing.T, toolCount uint8) {
		toolCount = uint8(int(toolCount)%200 + 1)

		store := NewMemoryTerminalEnvelopeStore(nil, nil)
		executor := &chainedEffectExecutor{}

		coordinator := newTestCoordinator(t, store, executor)

		// Bootstrap the turn.
		if err := coordinator.Submit(t.Context(), StartTurn{}); err != nil {
			t.Fatal(err)
		}
		if err := coordinator.Submit(t.Context(), PreparationFinished{}); err != nil {
			t.Fatal(err)
		}

		// Submit many tool calls to test deep chaining.
		calls := make([]ToolCallState, int(toolCount))
		for i := range calls {
			calls[i] = ToolCallState{
				ID:   fmt.Sprintf("call-%d", i),
				Name: "tool",
			}
		}

		err := coordinator.Submit(t.Context(), ToolCallsProposed{Calls: calls})
		if err != nil {
			t.Fatal(err)
		}

		// The recursion depth should be bounded (typically 1 since
		// SynchronousEffectDispatcher calls the submit callback directly
		// but the EffectResultReceived command doesn't produce more effects).
		if executor.maxDepth > 10 {
			t.Errorf("recursion depth %d exceeds safe bound of 10", executor.maxDepth)
		}
	})
}

// FuzzCoordinatorSubmitDoesNotDeadlock verifies that random sequences of
// valid commands submitted to the coordinator do not deadlock or panic.
func FuzzCoordinatorSubmitDoesNotDeadlock(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6, 7})
	f.Add([]byte{0, 1, 3, 4, 8, 9, 10, 11, 12})

	f.Fuzz(func(t *testing.T, input []byte) {
		store := NewMemoryTerminalEnvelopeStore(nil, nil)
		executor := &recordingEffectExecutor{
			result: func(effect Effect) Command {
				return EffectResultReceived{
					EffectID: effect.ID,
					Success:  true,
				}
			},
		}

		coordinator := newTestCoordinator(t, store, executor)

		// Start the turn.
		if err := coordinator.Submit(t.Context(), StartTurn{}); err != nil {
			t.Fatal(err)
		}

		// Submit random commands until the turn is terminal or we run out of input.
		const maxCommands = 256
		limit := min(len(input), maxCommands)
		for i := 0; i < limit; i++ {
			state := coordinator.Snapshot()
			if state.Phase.Terminal() {
				break
			}

			command := fuzzCoordinatorCommand(state, i, input[i])
			if command == nil {
				continue
			}

			err := coordinator.Submit(t.Context(), command)
			if err != nil {
				// Errors are expected for invalid transitions — just continue.
				continue
			}
		}

		// Verify the coordinator is still in a valid state.
		state := coordinator.Snapshot()
		if err := Validate(state); err != nil {
			t.Errorf("invalid state after fuzz: %v", err)
		}
	})
}

func fuzzCoordinatorCommand(state State, index int, value byte) Command {
	callID := fmt.Sprintf("call-%d", index)
	switch value % 14 {
	case 0:
		return PreparationFinished{}
	case 1:
		return ModelTextReceived{Text: "output"}
	case 2:
		return ToolCallsProposed{Calls: []ToolCallState{{
			ID: callID, Name: "tool",
		}}}
	case 3:
		for id := range state.OpenCalls {
			return ToolResultReceived{CallID: id}
		}
		return nil
	case 4:
		return VerificationRetired{}
	case 5:
		return VerificationRetired{}
	case 6:
		return CompletionEvaluated{Candidate: CompletionCandidate{
			DeclarationValid: true,
			Status:           "complete",
			Summary:          "fixture",
			CompletionCall:   callID,
			BatchSize:        1,
		}}
	case 7:
		return TerminalRequested{}
	case 8:
		status := JournalRolledBack
		if state.PendingTerminal != nil &&
			state.PendingTerminal.Kind == TerminalCompleted {
			status = JournalCommitted
		}
		return JournalFinalized{Status: status}
	case 9:
		return FinishTerminal{}
	case 10:
		return CancelRequested{Reason: "fixture cancel"}
	case 11:
		for effectID := range state.PendingEffects {
			return EffectStarted{EffectID: effectID, Attempt: 1}
		}
		return nil
	case 12:
		for effectID := range state.PendingEffects {
			return EffectResultReceived{EffectID: effectID, Success: true}
		}
		return nil
	default:
		return RepairRequested{
			Kind: RepairCompletion, ProgressKey: callID, Limit: 2,
		}
	}
}
