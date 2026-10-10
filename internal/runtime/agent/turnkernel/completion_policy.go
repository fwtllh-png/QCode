package turnkernel

import (
	"errors"
	"slices"
	"strings"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func effectiveChanges(state State) []ObservedChange {
	if state.Workspace != nil && state.Workspace.Mutation == state.MutationRevision {
		return state.Workspace.Changes
	}
	return state.Changes
}

func hasEffectiveChanges(state State) bool { return len(effectiveChanges(state)) != 0 }

// A declaration explains why a requested edit is unnecessary and cites actual
// successful reads from this turn. This checks provenance, not semantic truth.
func validNoChange(state State, reason string, calls []string) bool {
	if state.Workspace == nil || hasEffectiveChanges(state) || strings.TrimSpace(reason) == "" || len(calls) == 0 {
		return false
	}
	for _, id := range calls {
		call, ok := state.ClosedCalls[id]
		if !ok || call.IsError {
			return false
		}
		readFound := false
		for _, read := range state.WorkItem.KnownReads {
			if read.CallID == id {
				readFound = true
				break
			}
		}
		if !readFound {
			return false
		}
	}
	return true
}

func workspaceCompletionMissing(state State) bool {
	return state.Intent == protocol.TurnIntentWorkspaceChange && state.MutationRevision == 0 &&
		(state.Completion == nil || !state.Completion.Accepted ||
			!validNoChange(state, state.Completion.NoChangeReason, state.Completion.NoChangeEvidence))
}

// Shared by step selection, output release, final readiness and terminal
// validation. Journal/effect settlement remains specific to each phase.
func validateCompletionPolicy(state State) error {
	if workspaceCompletionMissing(state) {
		return errors.New("workspace_change requires observed changes or an evidenced no-change declaration")
	}
	if RequiresCompletion(state) {
		if state.Completion == nil || !state.Completion.Accepted {
			return errors.New("turn has no accepted completion decision")
		}
		if state.Completion.Mutation != state.MutationRevision {
			return errors.New("completion decision is stale")
		}
	}
	return nil
}

func applyWorkspaceReconciled(transition *Transition, current State, command WorkspaceReconciled) error {
	if err := requirePhase(current, command, PhaseSampling); err != nil {
		return err
	}
	if len(current.OpenCalls) != 0 || current.ActiveSampleID != "" || command.Mutation != current.MutationRevision {
		return illegal(current, command, "workspace observation is stale or work is still running")
	}
	if len(command.Changes) != 0 && current.MutationRevision == 0 {
		return illegal(current, command, "workspace changes have no observed mutation")
	}
	if current.Workspace != nil && !slices.Equal(current.Workspace.Changes, command.Changes) {
		// A snapshot may refine operation-level observations, but cannot silently
		// replace facts at the same revision after they have been reconciled.
		return illegal(current, command, "workspace changed without a new mutation")
	}
	transition.State.Workspace = &WorkspaceState{Mutation: command.Mutation, Changes: append([]ObservedChange(nil), command.Changes...)}
	if transition.State.Completion != nil {
		transition.State.Completion.ChangedPaths = changedPaths(command.Changes)
	}
	// Only discharge this turn's restored edits. Inherited session evidence
	// remains conservative; absence of a new edit does not verify old work.
	if len(command.Changes) == 0 {
		paths := changedPaths(current.Changes)
		transition.State.WorkItem.Open.UnverifiedPaths = slices.DeleteFunc(transition.State.WorkItem.Open.UnverifiedPaths, func(path string) bool {
			return slices.Contains(paths, path)
		})
	}
	transition.State.WorkItem.RequiredAction = DeriveRequiredAction(transition.State)
	return nil
}

func (s *RuntimeKernel) ReconcileWorkspace(changes []ObservedChange) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Workspace != nil && slices.Equal(s.state.Workspace.Changes, changes) {
		return nil
	}
	return s.applyAuthoritativeLocked(WorkspaceReconciled{Mutation: s.state.MutationRevision, Changes: changes})
}
