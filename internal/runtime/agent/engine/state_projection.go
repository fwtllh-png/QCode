package engine

import (
	"fmt"

	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
)

// The kernel Phase is the authoritative turn state machine. The host-facing
// State is a presentation refinement: sampling is split into compaction and
// transport detail, tool execution into preparation, running, and feeding.
//
// phaseStates is the single declaration of which host states may be observed
// while the kernel rests in each phase. Adding a Phase or a State constant
// without extending this table fails the source-level completeness check in
// state_projection_test.go, so the two vocabularies cannot drift apart
// silently.
//
// AwaitingApproval and AwaitingInput are emitted by the dedicated approval
// and input wait paths, after the kernel already entered the matching phase;
// every other state flows through turnEmitter.send, which enforces this
// table at emission time.
//
// The buckets record observed coexistence, not aspiration:
//
//   - Preparing is the turn-opening presentation event. A fresh kernel
//     reaches sampling during construction, and a resumed kernel restores
//     into whatever phase it was interrupted in, so Preparing may be
//     observed in any non-terminal phase.
//   - Tool execution and verification emissions overlap the neighbouring
//     kernel phases: a tool that waits on user input leaves the kernel in
//     awaiting_input while tool-batch events still project, tool effects
//     dispatch from sampling, and stream deltas may trail the phase move
//     back from executing_tools.
//   - Compacting also covers post-terminal context maintenance receipts
//     projected after the kernel already reached a terminal phase but
//     before the terminal event itself is emitted.
var phaseStates = map[turnkernel.Phase][]State{
	turnkernel.PhaseCreated:          {Preparing},
	turnkernel.PhasePreparing:        {Preparing, Compacting},
	turnkernel.PhaseSampling: {
		Preparing, Compacting, CallingModel, Streaming, PreparingTools,
		RunningTools, FeedingResults, Verifying,
	},
	turnkernel.PhaseExecutingTools: {
		Preparing, PreparingTools, RunningTools, FeedingResults, Streaming,
	},
	turnkernel.PhaseAwaitingApproval: {Preparing, AwaitingApproval},
	turnkernel.PhaseAwaitingInput:    {Preparing, AwaitingInput, RunningTools},
	turnkernel.PhaseVerifying:        {Preparing, Verifying},
	turnkernel.PhaseCommitting:       {Preparing, AwaitingRecovery},
	turnkernel.PhaseCompleted:        {Completed, Compacting},
	turnkernel.PhaseFailed:           {Failed, Compacting},
	turnkernel.PhaseCanceled:         {Canceled, Compacting},
}

// stateAllowedForPhase reports whether the host-facing state may be emitted
// while the authoritative kernel phase is phase.
func stateAllowedForPhase(state State, phase turnkernel.Phase) bool {
	for _, candidate := range phaseStates[phase] {
		if candidate == state {
			return true
		}
	}
	return false
}

func statePhaseMismatch(state State, phase turnkernel.Phase) error {
	return fmt.Errorf(
		"engine state %q is not declared for kernel phase %q",
		state, phase,
	)
}
