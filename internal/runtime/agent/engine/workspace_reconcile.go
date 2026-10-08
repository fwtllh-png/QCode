package engine

import (
	"sort"

	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
)

// Reconcile only after all tool calls close, before declaration evaluation or
// step selection. Journal before/after fingerprints take precedence over the
// operation-level diff; unjournaled observations remain conservative changes.
func (e *Engine) reconcileWorkspace(kernel *turnkernel.RuntimeKernel) error {
	scope := e.executionScope()
	if scope == nil || scope.state.diff == nil {
		return nil
	}
	entries := make(map[string]TurnDiffEntry)
	digests := make(map[string]string)
	for _, entry := range scope.state.diff.Snapshot() {
		if path, ok := agentcontext.WorkspaceRelative(e.options.Workspace, entry.Path); ok {
			entry.Path = path
		}
		entries[entry.Path] = entry
	}
	// The durable kernel owns mutation observations. Rebuild their net path
	// set even when a restored engine has no in-memory diff yet.
	state := kernel.Snapshot()
	observedDiff := turnkernel.NewTurnDiffTracker(e.options.Workspace)
	observations := state.Changes
	if state.Workspace != nil {
		observations = state.Workspace.Changes
	}
	for _, change := range observations {
		path := change.Path
		if relative, ok := agentcontext.WorkspaceRelative(e.options.Workspace, path); ok {
			path = relative
		}
		observedDiff.Record(TurnDiffEntry{Path: path, Kind: change.Kind})
		digests[path] = change.ContentDigest
	}
	presentation := entries
	entries = make(map[string]TurnDiffEntry)
	for _, change := range observedDiff.Snapshot() {
		entry := presentation[change.Path]
		entry.Path, entry.Kind = change.Path, change.Kind
		entries[entry.Path] = entry
	}
	if e.journal != nil {
		observed, err := e.journal.ObservedChanges(scope.spec.Identity.TurnID)
		if err != nil {
			return err
		}
		for _, change := range observed {
			path, ok := agentcontext.WorkspaceRelative(e.options.Workspace, change.Path)
			if !ok {
				continue
			}
			if change.Kind == "" {
				delete(entries, path)
				continue
			}
			entry := entries[path]
			entry.Path, entry.Kind = path, change.Kind
			entries[path] = entry
			digests[path] = change.After.SHA256
		}
	}
	diff := make([]TurnDiffEntry, 0, len(entries))
	for _, entry := range entries {
		diff = append(diff, entry)
	}
	sort.Slice(diff, func(i, j int) bool { return diff[i].Path < diff[j].Path })
	changes := make([]turnkernel.ObservedChange, 0, len(diff))
	for _, entry := range diff {
		changes = append(changes, turnkernel.ObservedChange{Path: entry.Path, Kind: entry.Kind, ContentDigest: digests[entry.Path]})
	}
	if err := kernel.ReconcileWorkspace(changes); err != nil {
		return err
	}
	scope.state.diff.Replace(diff)
	return nil
}
