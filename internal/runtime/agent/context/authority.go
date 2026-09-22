package agentcontext

import (
	"sort"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
)

// Authority owns the context state that must advance as one Session-level
// unit. Callers provide synchronization; Authority keeps cloning and restore
// semantics in the context owner instead of duplicating them in hosts.
type Authority struct {
	working    *WorkingSetLedger
	evidence   *EvidenceSet
	failures   *Failures
	world      WorldBaseline
	window     WindowLedger
	compaction Compaction
}

func NewAuthority() Authority {
	return Authority{
		working:  NewWorkingSet(),
		evidence: NewEvidenceSet(),
		failures: NewFailures(),
	}
}

func (a Authority) Clone() Authority {
	return Authority{
		working:    a.WorkingSet().Clone(),
		evidence:   a.Evidence().Clone(),
		failures:   a.Failures().Clone(),
		world:      CloneWorldBaseline(a.world),
		window:     CloneWindowLedger(a.window),
		compaction: CloneCompaction(a.compaction),
	}
}

func (a *Authority) WorkingSet() *WorkingSetLedger {
	if a.working == nil {
		a.working = NewWorkingSet()
	}
	return a.working
}

func (a *Authority) Evidence() *EvidenceSet {
	if a.evidence == nil {
		a.evidence = NewEvidenceSet()
	}
	return a.evidence
}

func (a *Authority) Failures() *Failures {
	if a.failures == nil {
		a.failures = NewFailures()
	}
	return a.failures
}

func (a Authority) World() WorldBaseline {
	return CloneWorldBaseline(a.world)
}

func (a *Authority) SetWorld(value WorldBaseline) {
	a.world = CloneWorldBaseline(value)
}

func (a *Authority) ReconcileWorld(history []provider.Message) {
	if !WorldBaselineValid(history, a.world) {
		a.world = WorldBaseline{}
	}
}

func (a Authority) Window() WindowLedger {
	return CloneWindowLedger(a.window)
}

func (a *Authority) SetWindow(value WindowLedger) {
	a.window = CloneWindowLedger(value)
}

func (a Authority) Compaction() Compaction {
	return CloneCompaction(a.compaction)
}

func (a *Authority) SetCompaction(value Compaction) {
	a.compaction = CloneCompaction(value)
}

func (a *Authority) NoteCompaction() {
	a.compaction.Count++
}

func (a *Authority) Restore(
	working WorkingSetDelta,
	evidence EvidenceDelta,
	failures FailureDelta,
	compaction Compaction,
	world WorldBaseline,
	window WindowLedger,
	history []provider.Message,
) {
	a.working = ApplyWorkingSetDelta(working)
	a.evidence = ApplyEvidenceDelta(evidence)
	a.failures = ApplyFailureDelta(failures)
	a.compaction = CloneCompaction(compaction)
	a.SetWorld(world)
	a.ReconcileWorld(history)
	a.SetWindow(window)
}

type SnapshotRequest struct {
	History             []provider.Message
	HistoryTurns        map[string]uint64
	Plan                Plan
	Turn                uint64
	Revision            uint64
	Epoch               uint64
	WorkingSetLimit     int
	TruthMaxEntities    int
	FactMaxEntities     int
	VerifiedChangeTurns uint64
	HandleMaxEntities   int
	WorkspaceRoot       string
	WorkspaceIdentity   string
	WorkspaceRevision   uint64
	TurnCheckpoints     []TurnCheckpoint
}

func (a Authority) Snapshot(request SnapshotRequest) (ContextSnapshot, error) {
	working := a.WorkingSet().RetainedDelta(
		request.Turn,
		request.WorkingSetLimit,
		request.TruthMaxEntities,
	)
	evidence := a.Evidence().RetainedDelta(
		request.FactMaxEntities,
		request.VerifiedChangeTurns,
		request.HandleMaxEntities,
	)
	workspace, err := CaptureWorkspaceBinding(
		request.WorkspaceRoot,
		request.WorkspaceIdentity,
		request.WorkspaceRevision,
		evidencePaths(evidence),
	)
	if err != nil {
		return ContextSnapshot{}, err
	}
	plan := request.Plan.Clone()
	snapshot := ContextSnapshot{
		Version:         ContextSnapshotVersion,
		Epoch:           request.Epoch,
		Revision:        request.Revision,
		Turn:            request.Turn,
		History:         CloneMessages(request.History),
		HistoryTurns:    CloneHistoryTurns(request.HistoryTurns),
		WorkingSet:      working,
		Evidence:        evidence,
		Failures:        a.Failures().Delta(),
		Compaction:      durableCompaction(a.Compaction()),
		Plan:            &plan,
		World:           a.World(),
		Workspace:       workspace,
		Window:          a.Window(),
		TurnCheckpoints: CloneTurnCheckpoints(request.TurnCheckpoints),
	}
	if err := snapshot.Seal(); err != nil {
		return ContextSnapshot{}, err
	}
	return snapshot, nil
}

func evidencePaths(delta EvidenceDelta) []string {
	paths := make(map[string]struct{})
	// Workspace bindings are workspace-relative by contract. Durable
	// evidence written before that rule (or by a tool reporting host
	// locations) can carry absolute paths; one malformed entry must not
	// reject every later snapshot, so unbindable paths are skipped.
	collect := func(path string) {
		if _, err := canonicalBoundPath(path); err == nil {
			paths[path] = struct{}{}
		}
	}
	for _, fact := range delta.Facts {
		collect(fact.Path)
	}
	for _, change := range delta.Changes {
		collect(change.Path)
	}
	for _, read := range delta.Reads {
		collect(read.Path)
	}
	result := make([]string, 0, len(paths))
	for path := range paths {
		result = append(result, path)
	}
	sort.Strings(result)
	return result
}

func durableCompaction(value Compaction) Compaction {
	value.DropInvalidState()
	return value
}

func CaptureWorkspaceBindingForEvidence(
	root string,
	identity string,
	revision uint64,
	evidence EvidenceDelta,
) (WorkspaceBinding, error) {
	return CaptureWorkspaceBinding(
		root,
		identity,
		revision,
		evidencePaths(evidence),
	)
}
