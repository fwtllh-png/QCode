// Package app pending-work routing: steer / mailbox / approval / input →
// inject into the current turn, resume a pause, start a new turn, buffer, or reject.
package app

import "fmt"

// PendingSource is one of the four unified pending-work origins (W3.2).
type PendingSource string

const (
	SourceSteer    PendingSource = "steer"
	SourceMailbox  PendingSource = "mailbox"
	SourceApproval PendingSource = "approval"
	SourceInput    PendingSource = "input"
)

// TurnPhase is the thread/turn activity the router observes.
type TurnPhase string

const (
	PhaseIdle             TurnPhase = "idle"
	PhaseRunning          TurnPhase = "running"
	PhaseAwaitingApproval TurnPhase = "awaiting_approval"
	PhaseAwaitingInput    TurnPhase = "awaiting_input"
)

// PendingDisposition is the scheduler decision for one pending item.
type PendingDisposition string

const (
	// DispositionInjectCurrent feeds text into the active turn (Engine.Steer / pending).
	DispositionInjectCurrent PendingDisposition = "inject_current"
	// DispositionResumePaused unblocks Guard/Interact Stage+Resume.
	DispositionResumePaused PendingDisposition = "resume_paused"
	// DispositionStartNewTurn wakes an idle thread with a fresh StartTurn.
	DispositionStartNewTurn PendingDisposition = "start_new_turn"
	// DispositionBuffer holds work until the next delivery window (mailbox queue-only).
	DispositionBuffer PendingDisposition = "buffer"
	// DispositionReject fails closed (unknown pause, wrong phase, etc.).
	DispositionReject PendingDisposition = "reject"
)

// PendingItem is the scheduler input. TriggerTurn applies to mailbox only.
type PendingItem struct {
	Source      PendingSource
	TriggerTurn bool
}

// RoutePending applies the W3.2 rule table.
//
//	Source \ Phase     | running           | awaiting_approval | awaiting_input | idle
//	-------------------|-------------------|-------------------|----------------|----------------
//	steer              | inject_current    | inject_current    | inject_current | start_new_turn
//	mailbox+trigger    | inject_current    | buffer            | buffer         | start_new_turn
//	mailbox            | buffer            | buffer            | buffer         | buffer
//	approval           | reject            | resume_paused     | reject         | reject
//	input              | reject            | reject            | resume_paused  | reject
func RoutePending(phase TurnPhase, item PendingItem) PendingDisposition {
	switch item.Source {
	case SourceSteer:
		switch phase {
		case PhaseIdle:
			return DispositionStartNewTurn
		case PhaseRunning, PhaseAwaitingApproval, PhaseAwaitingInput:
			return DispositionInjectCurrent
		default:
			return DispositionReject
		}
	case SourceMailbox:
		if !item.TriggerTurn {
			return DispositionBuffer
		}
		switch phase {
		case PhaseIdle:
			return DispositionStartNewTurn
		case PhaseRunning:
			return DispositionInjectCurrent
		case PhaseAwaitingApproval, PhaseAwaitingInput:
			return DispositionBuffer
		default:
			return DispositionReject
		}
	case SourceApproval:
		if phase == PhaseAwaitingApproval {
			return DispositionResumePaused
		}
		return DispositionReject
	case SourceInput:
		if phase == PhaseAwaitingInput {
			return DispositionResumePaused
		}
		return DispositionReject
	default:
		return DispositionReject
	}
}

// ExplainPending returns a stable reason string for logs/tests.
func ExplainPending(phase TurnPhase, item PendingItem, disposition PendingDisposition) string {
	return fmt.Sprintf("pending source=%s phase=%s trigger_turn=%v → %s",
		item.Source, phase, item.TriggerTurn, disposition)
}
