package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/persist/workspacejournal"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// Execute is the only production entry point for a Turn. It snapshots every
// mutable Session dependency before opening the execution Scope.
func (e *Engine) Execute(
	ctx context.Context,
	request TurnRequest,
	emit func(Event) error,
) (result Result, resultErr error) {
	e.mu.Lock()
	providerGate := e.options.SharedRateLimit
	e.mu.Unlock()
	releasePriority := providerGate.BeginForegroundTurn(ctx)
	defer releasePriority()
	// Join before taking e.mu: the pending narrative settles under e.mu, so
	// waiting while holding it would deadlock with the settling goroutine.
	e.joinPendingNarrative()
	e.mu.Lock()
	defer e.mu.Unlock()
	e.syncSessionTitleState(provider.Usage{})
	spec, persistedTurnID, err := e.prepareTurnSpec(
		ctx,
		request,
	)
	if err != nil {
		return Result{}, err
	}
	if e.options.TurnContexts != nil {
		// Admission must survive an immediate Stop: the accepted request still
		// needs its baseline before the canceled Scope publishes a terminal.
		baselineContext := context.WithoutCancel(ctx)
		threadID := protocol.ThreadID(spec.Identity.ThreadID)
		turnID := protocol.TurnID(spec.Identity.TurnID)
		if e.journal != nil {
			for _, draft := range e.journal.DraftTurnIDs() {
				withdrawn, checkErr := e.options.TurnContexts.TurnWithdrawn(baselineContext, threadID, protocol.TurnID(draft))
				if checkErr != nil {
					return Result{}, checkErr
				}
				if withdrawn {
					if keepErr := e.keepWithdrawnDraft(draft); keepErr != nil {
						return Result{}, keepErr
					}
				}
			}
		}
		withdrawn, checkErr := e.options.TurnContexts.TurnWithdrawn(baselineContext, threadID, turnID)
		if checkErr != nil {
			return Result{}, checkErr
		}
		if withdrawn {
			return Result{}, protocol.NewProblem(protocol.CodeConflict, "Turn was withdrawn", false, nil)
		}
		if _, found, loadErr := e.options.TurnContexts.TurnBaseline(baselineContext, threadID, turnID); loadErr != nil {
			return Result{}, loadErr
		} else if !found {
			baseline, snapshotErr := e.buildContextSnapshot(e.history, e.context.Compaction(),
				max(uint64(1), e.sessionRevision), max(uint64(1), e.stateEpoch))
			if snapshotErr != nil {
				return Result{}, snapshotErr
			}
			if saveErr := e.options.TurnContexts.SaveTurnBaseline(baselineContext, threadID, turnID, baseline); saveErr != nil {
				return Result{}, saveErr
			}
		}
	}
	factory := scopeFactory{
		engine: e, emit: emit, persistedTurnID: persistedTurnID,
	}
	scope, err := factory.Open(ctx, spec)
	if err != nil {
		return Result{}, err
	}
	defer scope.Close(context.WithoutCancel(ctx))
	return scope.Run(ctx)
}

func (e *Engine) prepareTurnSpec(
	ctx context.Context,
	request TurnRequest,
) (TurnSpec, string, error) {
	persistedTurnID := request.TurnID
	if request.Prompt == "" {
		return TurnSpec{}, "", errors.New("prompt is required")
	}
	request.Intent = protocol.NormalizeTurnIntent(request.Intent)
	if !request.Intent.Valid() {
		return TurnSpec{}, "", protocol.NewProblem(
			protocol.CodeInvalidArgument,
			fmt.Sprintf("turn intent %q is invalid", request.Intent),
			false,
			nil,
		)
	}
	if request.Recovery != nil {
		if err := request.Recovery.Validate(); err != nil {
			return TurnSpec{}, "", protocol.NewProblem(
				protocol.CodeInvalidArgument,
				err.Error(),
				false,
				err,
			)
		}
	}
	if request.TurnID == "" {
		request.TurnID = fmt.Sprintf("engine-turn-%d", e.turn+1)
	}
	spec, err := SnapshotTurnSpec(
		e.options,
		TurnIdentity{
			SessionID:       e.options.SessionID,
			ThreadID:        tool.InvocationIdentityFrom(ctx).ThreadID,
			TurnID:          request.TurnID,
			ProfileRevision: e.options.ProfileRevision,
		},
		request,
	)
	if err != nil {
		return TurnSpec{}, "", err
	}
	spec.World = e.context.World()
	spec.Window = e.context.Window()
	e.applyImplementProgressLease(&spec)
	return spec, persistedTurnID, nil
}

type executionScope = turnkernel.Lifecycle[
	TurnSpec,
	Result,
	ScopeSnapshot,
	ControlPort,
]

type scopeFactory struct {
	engine          *Engine
	emit            func(Event) error
	persistedTurnID string
}

func (f scopeFactory) Open(
	_ context.Context,
	spec TurnSpec,
) (*executionScope, error) {
	return f.open(spec)
}

func (f scopeFactory) open(spec TurnSpec) (*executionScope, error) {
	if f.engine == nil {
		return nil, errors.New("turn scope engine is required")
	}
	emit := f.emit
	if emit == nil {
		emit = func(Event) error { return nil }
	}
	scope := &Scope{
		engine: f.engine, spec: spec, emit: emit,
		persistedTurnID: f.persistedTurnID,
		state:           newScopeState(f.engine),
	}
	scope.state.context.SetWorld(spec.World)
	scope.state.context.SetWindow(spec.Window)
	f.engine.publishScope(scope)
	f.engine.attachPending(scope)
	return turnkernel.NewLifecycle(
		scope.Spec(),
		scope.Run,
		scope.Control(),
		scope.Snapshot,
		func(context.Context) error { scope.Close(); return nil },
	)
}

// Run owns one frozen TurnSpec.
func (s *Scope) Run(ctx context.Context) (result Result, resultErr error) {
	e := s.engine
	spec := s.spec
	emit := s.emit
	turnID := spec.Identity.TurnID
	intent := spec.Request.Intent
	persistedTurnID := s.persistedTurnID
	releaseWorkspace, err := e.options.WorkspaceTurnGate.Acquire(ctx)
	if err != nil {
		return Result{}, err
	}
	defer releaseWorkspace()

	ctx, recorder, turnSpan := e.beginTrace(
		ctx,
		spec.Purpose,
		spec.Identity,
	)
	defer func() {
		e.endTrace(context.WithoutCancel(ctx), recorder, turnSpan, persistedTurnID, result.State)
	}()
	draftTurnID := ""
	if e.journal != nil &&
		spec.Request.Recovery != nil &&
		spec.Request.Recovery.Action == protocol.TurnRecoveryContinue {
		sourceTurnID := string(spec.Request.Recovery.SourceTurnID)
		switch {
		case e.journal.HasDraft(sourceTurnID):
			draftTurnID = sourceTurnID
		case e.journal.HasDraft(turnID):
			// A restarted recovery Turn owns the same draft under its new ID.
			draftTurnID = turnID
		default:
			// The owning Session was deleted; Continue adopts the leftover draft.
			draftTurnID = e.orphanedDraftTurnID(ctx)
		}
	}
	draftResumed := draftTurnID != ""
	var (
		draftChanges       []workspacejournal.Change
		kernelDraftChanges []turnkernel.ObservedChange
	)
	if draftResumed {
		draftChanges = e.journal.DraftChanges(draftTurnID)
		kernelDraftChanges = make(
			[]turnkernel.ObservedChange,
			0,
			len(draftChanges),
		)
		for _, change := range draftChanges {
			if relative, ok := agentcontext.WorkspaceRelative(e.options.Workspace, change.Path); ok {
				change.Path = relative
			}
			kernelDraftChanges = append(
				kernelDraftChanges,
				turnkernel.ObservedChange{
					Path: change.Path, Kind: change.Kind,
				},
			)
		}
	}
	kernel, err := turnkernel.NewRuntimeKernel(
		turnkernel.KernelIdentity{
			TurnID:          turnID,
			ProfileRevision: spec.Identity.ProfileRevision,
			Goal:            e.workItemGoal(spec),
			WorkItem:        e.continueWorkItemSeed(spec),
		},
		intent,
		string(spec.Mode),
		spec.Request.Recovery,
		draftResumed,
		kernelDraftChanges,
		kernelTransitionObserver(recorder, turnSpan.ID()),
		e.options.TurnKernelObserver,
		nil,
		e.options.Metrics,
		spec.Kernel,
		e.options.TurnCoordinatorRuntime,
	)
	if err != nil {
		return result, err
	}
	s.mu.Lock()
	s.state.kernel = kernel
	s.mu.Unlock()
	releasedCoordinator := false
	releaseCoordinator := func() error {
		if releasedCoordinator {
			return nil
		}
		if err := e.options.TurnCoordinatorRuntime.Release(
			context.WithoutCancel(ctx),
			turnID,
		); err != nil {
			return err
		}
		releasedCoordinator = true
		return nil
	}
	var terminal *turnEmitter
	defer func() {
		if err := releaseCoordinator(); err != nil {
			if terminal != nil {
				terminal.addReleaseIssue(err)
			}
			if terminal == nil || !terminal.emitted {
				resultErr = errors.Join(resultErr, err)
			}
		}
	}()
	if e.guard != nil && spec.Policy != nil {
		sessionPolicy := e.guard.SwapPolicy(spec.Policy)
		defer e.guard.SwapPolicy(sessionPolicy)
	}
	e.setApprovalEmit(func(event Event) error {
		event.State, event.Turn = AwaitingApproval, e.turn
		return emit(event)
	})
	defer e.setApprovalEmit(nil)
	disconnectInput := e.connectInputHost(kernel, emit)
	defer disconnectInput()
	e.turn++
	result.Turn = e.turn
	e.evidenceSet().BeginTurn(e.turn)
	_, restoredTerminal := kernel.TerminalDecision()
	if e.journal != nil && !restoredTerminal {
		var journalErr error
		switch {
		case draftResumed:
			journalErr = e.journal.ResumeDraft(draftTurnID, turnID)
		default:
			if retryDraftID := e.retryDraftTurnID(ctx, spec); retryDraftID != "" {
				_, journalErr = e.journal.Revert(
					context.Background(),
					retryDraftID,
				)
				if journalErr == nil {
					journalErr = e.journal.Begin(turnID)
				}
			} else {
				journalErr = e.journal.Begin(turnID)
			}
		}
		if journalErr != nil {
			// TurnStarted must exist before journal admission fails, otherwise
			// Retry/Continue cannot recover this Turn.
			_ = emit(Event{
				State:              Preparing,
				Provider:           spec.Provider,
				Model:              spec.Model,
				ModelMetadata:      spec.ModelMetadata,
				Purpose:            string(spec.Purpose),
				ProfileRevision:    spec.Identity.ProfileRevision,
				Mode:               string(spec.Mode),
				Posture:            string(spec.Posture),
				Workspace:          spec.Workspace,
				WorkspaceIsolation: e.options.WorkspaceIsolation,
				Sandbox:            spec.Sandbox,
			})
			problem := e.journalAdmissionProblem(ctx, journalErr)
			if terminalErr := kernel.FailBeforeJournal(
				context.Background(),
				problem,
			); terminalErr != nil {
				return result, errors.Join(journalErr, terminalErr)
			}
			result.State = Failed
			return result, problem
		}
		for _, change := range draftChanges {
			s.state.diff.Record(turnkernel.TurnDiffEntry{
				Path: change.Path, Tool: "recovery_draft", Kind: change.Kind,
			})
			e.contextAuthority().ObservePath(
				e.options.Workspace,
				agentcontext.SourceEdited,
				e.turn,
				change.Path,
			)
			e.contextAuthority().ObserveChange(
				e.options.Workspace,
				tool.WorkspaceChange{
					Path: change.Path, Kind: change.Kind,
				},
				e.turn,
			)
		}
	}
	transaction := agentcontext.RecoveryBaseHistory(e.history, e.historyTurns, spec.Request.Recovery)
	continuation, continuationUsable, continuationErr := e.loadTurnContinuation(ctx, kernel, spec)
	terminal = newTurnEmitter(e.turn, emit)
	terminal.setCommitted(e.applySessionDelta)
	if continuationErr != nil {
		terminal.addSecondary("turn_continuation", continuationErr)
	}
	terminal.setCancelReason(func() string {
		if reason := kernel.CancellationReason(); reason != "" {
			return reason
		}
		return e.cancellationReason()
	})
	terminal.setTerminalDecision(kernel.TerminalDecision)
	terminal.setPhase(kernel.Phase)
	terminal.setRelease(releaseCoordinator)
	run := newTurnRun(ctx, s, kernel, terminal, transaction, &result)
	defer run.settle(&resultErr)
	resultErr = run.execute(continuation, continuationUsable)
	return result, resultErr
}

func errorText(err error) string {
	if err == nil {
		return "turn failed"
	}
	return err.Error()
}
