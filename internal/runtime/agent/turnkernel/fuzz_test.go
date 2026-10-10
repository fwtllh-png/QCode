package turnkernel

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func FuzzReducerPreservesInvariants(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6, 7})
	f.Add([]byte{0, 1, 3, 4, 8, 9, 10})
	f.Add([]byte{0, 1, 11, 12})

	f.Fuzz(func(t *testing.T, input []byte) {
		const maxCommands = 1024
		if len(input) > maxCommands {
			input = input[:maxCommands]
		}
		state := NewState(protocol.TurnIntentAnswer, "act", 1)
		for index, value := range input {
			if state.Phase.Terminal() {
				_, err := (Reducer{}).Apply(
					state,
					ModelTextReceived{Text: "post-terminal"},
				)
				if !errors.Is(err, ErrIllegalTransition) {
					t.Fatalf("terminal transition error = %v", err)
				}
				return
			}
			command := fuzzCommand(state, index, value)
			before, err := Digest(state)
			if err != nil {
				t.Fatal(err)
			}
			transition, applyErr := (Reducer{}).Apply(state, command)
			if applyErr != nil {
				after, digestErr := Digest(state)
				if digestErr != nil {
					t.Fatal(digestErr)
				}
				if before != after {
					t.Fatalf("rejected command %T mutated state", command)
				}
				continue
			}
			if err := Validate(transition.State); err != nil {
				t.Fatalf("command %T produced invalid state: %v", command, err)
			}
			state = transition.State
		}
	})
}

func fuzzCommand(state State, index int, value byte) Command {
	callID := fmt.Sprintf("call-%d", index)
	switch value % 23 {
	case 0:
		return StartTurn{}
	case 1:
		return PreparationFinished{}
	case 2:
		return ModelTextReceived{Text: "output"}
	case 3:
		return ToolCallsProposed{Calls: []ToolCallState{{
			ID: callID, Name: "fixture_tool",
		}}}
	case 4:
		for id := range state.OpenCalls {
			return ToolResultReceived{CallID: id}
		}
		return ToolResultReceived{CallID: callID}
	case 5:
		return VerificationRetired{}
	case 6:
		return VerificationRetired{}
	case 7:
		return CompletionEvaluated{Candidate: CompletionCandidate{
			DeclarationValid: true,
			Status:           "complete",
			Summary:          "fixture",
			CompletionCall:   callID,
			BatchSize:        1,
		}}
	case 8:
		return TerminalRequested{}
	case 9:
		return TerminalRequested{FailureMessage: "fixture failure"}

	case 10:
		return TerminalRequested{CancelReason: "fixture canceled"}

	case 11:
		status := JournalRolledBack
		if state.PendingTerminal != nil &&
			state.PendingTerminal.Kind == TerminalCompleted {
			status = JournalCommitted
		}
		return JournalFinalized{Status: status}
	case 12:
		return FinishTerminal{}
	case 13:
		return ReleaseProvisionalOutput{}
	case 14:
		return DiscardProvisionalOutput{Reason: "fixture discard"}
	case 15:
		return RepairRequested{
			Kind: RepairCompletion, ProgressKey: callID, Limit: 2,
		}
	case 16:
		return ModelSampleStarted{SampleID: callID, Attempt: 1}
	case 17:
		return ModelSampleFinished{SampleID: state.ActiveSampleID}
	case 18:
		effectID := ""
		attempt := uint32(1)
		retry := uint32(1)
		if sample, ok := state.SampleLedger[state.ActiveSampleID]; ok {
			attempt = sample.Attempt
			retry = sample.ProviderRetries + 1
		}
		for id, effect := range state.PendingEffects {
			if effect.Kind == EffectSampleProvider &&
				effect.CallID == state.ActiveSampleID {
				effectID = id
				break
			}
		}
		return ProviderRetryRequested{
			EffectID: effectID, SampleID: state.ActiveSampleID,
			Attempt: attempt, Retry: retry,
			Failure: provider.Failure{
				Code:    provider.FailureTransport,
				Message: "fixture retry",
			},
			RetryAt: time.Unix(1, 0), PolicyRevision: "fuzz/v1",
		}
	case 19:
		return CancelRequested{Reason: "fixture cancel"}
	case 20:
		for effectID, effect := range state.PendingEffects {
			return EffectStarted{EffectID: effectID, Attempt: effect.Attempt + 1}
		}
		return EffectStarted{EffectID: callID, Attempt: 1}
	case 21:
		for effectID := range state.PendingEffects {
			return EffectResultReceived{EffectID: effectID, Success: true}
		}
		return EffectResultReceived{EffectID: callID, Success: true}
	default:
		return RecoveryRequested{
			SourceTurnID:           "source",
			RecoveryTurnID:         callID,
			CurrentProfileRevision: 2,
		}
	}
}
