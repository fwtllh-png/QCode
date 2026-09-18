package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	providerassembly "github.com/fwtllh-png/QCode/internal/adapter/provider/assembly"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/observability/verify"
	"github.com/fwtllh-png/QCode/internal/persist/workspacejournal"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	promptcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/prompt"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	"github.com/fwtllh-png/QCode/internal/security/policy"
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
	prompt := spec.Request.Prompt
	intent := spec.Request.Intent
	attachments := spec.Request.Attachments
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
	kernelTerminalFinalized := false
	kernelTerminalStarted := false
	journalRevert := false
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
			kernelTerminalStarted = true
			kernelTerminalFinalized = true
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
	send := terminal.send
	defer terminal.finish(ctx, &result, &resultErr)
	for _, message := range kernel.CommentaryMessages() {
		terminal.publishCommentary(&message)
	}
	contextFinalized := false
	defer func() {
		if contextFinalized {
			return
		}
		canceled := errors.Is(resultErr, context.Canceled) ||
			errors.Is(ctx.Err(), context.Canceled)
		var decision *policy.DecisionError
		if errors.As(resultErr, &decision) &&
			decision.Code == "approval_canceled" {
			canceled = true
		}
		if canceled &&
			e.cancellationReason() != protocol.CancelReasonUserInterrupted {
			canceled = false
		}
		terminal.setPrimary(resultErr)
		snapshot, err := e.finalizeTerminalContext(
			transaction, false, canceled, resultErr, provider.Usage{}, 0, send,
		)
		terminal.setContextBudget(snapshot)
		if err != nil {
			terminal.addSecondary("terminal_context", err)
			resultErr = errors.Join(resultErr, err)
		}
	}()
	finalizeKernel := func(
		request turnkernel.TerminalRequested,
		resumed *turnkernel.TerminalDecision,
	) error {
		if kernelTerminalFinalized {
			return nil
		}
		journal := turnkernel.JournalDriver{}
		if e.journal != nil {
			journal.Commit = func() error {
				return e.journal.Commit(turnID)
			}
			journal.Suspend = func() error {
				err := e.journal.Suspend(turnID)
				if result.Verification != nil {
					result.Verification.Workspace = &VerificationWorkspace{
						Status: "draft",
						Note:   "workspace changes are retained as a resumable, unverified draft",
					}
				}
				return err
			}
			journal.Rollback = func() error {
				receipt, err := e.journal.Rollback(
					context.Background(),
					turnID,
				)
				if result.Verification != nil {
					result.Verification.Workspace = verify.WorkspaceFromJournal(
						receipt,
					)
				}
				e.recordRollbackConflicts(receipt)
				return err
			}
		}
		finalized, err := kernel.FinalizeTerminal(
			request,
			resumed,
			journal,
		)
		kernelTerminalStarted = kernelTerminalStarted || finalized.Started
		kernelTerminalFinalized = finalized.Finalized
		if err != nil {
			return err
		}
		if finalized.Pending != nil {
			terminal.suspendForRecovery()
			result.State = AwaitingRecovery
			fault := protocol.NewFault(
				protocol.CodeUnavailable,
				"workspace journal finalization is awaiting recovery",
				true,
				protocol.FaultMetadata{
					Origin:         protocol.FaultOriginPersistence,
					Disposition:    protocol.FaultRetryStep,
					SideEffects:    protocol.SideEffectUnknown,
					RecoveryAction: "retry the pending idempotent journal effect",
				},
				finalized.Pending,
			)
			projectionErr := send(AwaitingRecovery, Event{
				ErrorCode: fault.Code,
				Error:     fault.Message,
				Fault:     fault.Fault,
			})
			return errors.Join(fault, projectionErr)
		}
		return nil
	}
	finishAcceptedCancellation := func() (bool, error) {
		reason := kernel.CancellationReason()
		if reason == "" {
			return false, nil
		}
		// The terminal commit must carry the closed conversation, so the
		// settlement has to run before the kernel finalizes: staging the
		// SessionDelta here is what keeps published-but-unprojected tool
		// results recoverable instead of vanishing with the canceled batch.
		snapshot, settleErr := e.finalizeTerminalContext(
			transaction, false, true, nil, provider.Usage{}, 0, send,
		)
		contextFinalized = true
		terminal.setContextBudget(snapshot)
		if settleErr != nil {
			terminal.addSecondary("terminal_context", settleErr)
		}
		if err := finalizeKernel(
			turnkernel.TerminalRequested{CancelReason: reason},
			nil,
		); err != nil {
			return true, err
		}
		result.State = Canceled
		return true, send(Canceled, Event{CancelReason: reason})
	}
	defer func() {
		if kernelTerminalFinalized || kernelTerminalStarted {
			return
		}
		_, _, request := terminal.terminalRequest(ctx, resultErr)
		if err := finalizeKernel(request, nil); err != nil {
			terminal.addSecondary("journal", err)
			resultErr = errors.Join(resultErr, err)
		}
	}()
	if decision, resuming := kernel.CommittingDecision(); resuming {
		kernelTerminalStarted = true
		contextFinalized = true
		terminal.setContextBudget(ContextBudgetSnapshot{})
		if err := finalizeKernel(
			turnkernel.TerminalRequested{},
			&decision,
		); err != nil {
			return result, err
		}
		switch decision.Kind {
		case turnkernel.TerminalCompleted:
			result.Text = kernel.FrozenOutput()
			result.State = Completed
			if err := send(Completed, Event{Text: result.Text}); err != nil {
				return result, err
			}
			return result, nil
		case turnkernel.TerminalCanceled:
			result.State = Canceled
			if err := send(Canceled, Event{
				CancelReason: decision.Message,
			}); err != nil {
				return result, err
			}
			return result, nil
		default:
			result.State = Failed
			if err := send(Failed, Event{Error: decision.Message}); err != nil {
				return result, err
			}
			return result, nil
		}
	}
	if decision, terminalized := kernel.TerminalDecision(); terminalized {
		kernelTerminalStarted = true
		kernelTerminalFinalized = true
		contextFinalized = true
		terminal.setContextBudget(ContextBudgetSnapshot{})
		switch decision.Kind {
		case turnkernel.TerminalCompleted:
			result.Text = kernel.FrozenOutput()
			result.State = Completed
			if err := send(Completed, Event{Text: result.Text}); err != nil {
				return result, err
			}
		case turnkernel.TerminalCanceled:
			result.State = Canceled
			if err := send(Canceled, Event{
				CancelReason: decision.Message,
			}); err != nil {
				return result, err
			}
		default:
			result.State = Failed
			if err := send(Failed, Event{
				ErrorCode: protocol.ErrorCode(decision.Code),
				Error:     decision.Message,
			}); err != nil {
				return result, err
			}
		}
		return result, nil
	}
	if kernel.CancellationReason() != "" {
		return result, context.Canceled
	}
	if err := send(Preparing, Event{
		Provider: spec.Provider, Model: spec.Model,
		ModelMetadata:   spec.ModelMetadata,
		Purpose:         string(spec.Purpose),
		ProfileRevision: spec.Identity.ProfileRevision,
		Mode:            string(spec.Mode), Posture: string(spec.Posture),
		Workspace:          spec.Workspace,
		WorkspaceIsolation: e.options.WorkspaceIsolation,
		Sandbox:            spec.Sandbox,
	}); err != nil {
		return result, err
	}
	if continuationUsable {
		// A restored Turn's accepted conversation already contains its
		// opening user request; re-appending the submitted prompt would
		// duplicate the goal the continuation carries.
		transaction = append(transaction, continuation.Messages...)
	} else {
		user := provider.TextMessage(provider.RoleUser, prompt)
		for index := range attachments {
			attachment := attachments[index]
			user.Blocks = append(user.Blocks, provider.ContentBlock{
				Type: provider.ContentImage, Attachment: &attachment,
			})
		}
		user.Turn = e.turn
		transaction = append(transaction, user)
	}
	executed := make(map[string]tool.Result)
	cache := &toolResultCache{}
	// salvageCompletedResults projects the durably closed calls of an
	// interrupted batch into the transaction. Calls without a kernel-closed
	// result stay dangling and are dropped by pair normalization during
	// settlement, so the model never sees an unclosed tool pair but the
	// completed evidence survives the cancellation.
	salvageCompletedResults := func(calls []provider.ToolCall) {
		if kernel.CancellationReason() == "" {
			return
		}
		completedCalls := make([]provider.ToolCall, 0, len(calls))
		completedResults := make([]tool.Result, 0, len(calls))
		for _, call := range calls {
			if result, ok := executed[call.ID]; ok {
				completedCalls = append(completedCalls, call)
				completedResults = append(completedResults, result)
			}
		}
		if len(completedCalls) == 0 {
			return
		}
		messages, err := tool.ProjectModelResults(
			completedCalls,
			completedResults,
			e.turn,
		)
		if err != nil {
			return
		}
		transaction = append(transaction, messages...)
	}
	// persistContinuation stores the accepted in-turn conversation at
	// semantic boundaries. The content is written before the kernel commits
	// the cursor, so a committed continuation fact always resolves and a
	// crash between the two leaves only an unreferenced blob for storage
	// governance to collect.
	persistContinuation := func(sampleID string, step int) {
		blobs := e.options.TurnContinuations
		if blobs == nil || !e.continuationEnvironmentComplete(spec) {
			return
		}
		messages, _, err := agentcontext.NormalizePairs(
			currentTurnMessages(transaction, e.turn),
		)
		if err != nil {
			terminal.addSecondary("turn_continuation", err)
			return
		}
		// Pair normalization projects through the ledger partition, which
		// drops turn attribution; restamp so restore-side filtering by the
		// current turn keeps working.
		for index := range messages {
			messages[index].Turn = e.turn
		}
		if len(messages) == 0 {
			return
		}
		record := agentcontext.TurnContinuation{
			Version:           agentcontext.ContinuationVersion,
			TurnID:            turnID,
			Sequence:          kernel.NextContinuationSequence(),
			TurnNumber:        e.turn,
			SessionRevision:   e.sessionRevision,
			StateEpoch:        max(uint64(1), e.stateEpoch),
			SampleID:          sampleID,
			Step:              step,
			WorkspaceIdentity: e.options.WorkspaceIdentity,
			ProfileRevision:   spec.Identity.ProfileRevision,
			Provider:          spec.Provider,
			Model:             spec.Model,
			Messages:          messages,
		}
		ref, err := agentcontext.StoreTurnContinuation(ctx, blobs, record)
		if err != nil {
			terminal.addSecondary("turn_continuation", err)
			return
		}
		if err := kernel.RecordContinuation(turnkernel.ContinuationCursor{
			Sequence: record.Sequence,
			Handle:   ref.Handle,
			Digest:   ref.Digest,
		}); err != nil {
			terminal.addSecondary("turn_continuation", err)
		}
	}
	progress := kernel.ProgressObservation()
	if recoveredCalls := kernel.PendingToolCalls(); len(recoveredCalls) != 0 {
		blocks := make([]provider.ContentBlock, 0, len(recoveredCalls))
		for _, call := range recoveredCalls {
			callCopy := call
			blocks = append(blocks, provider.ContentBlock{
				Type: provider.ContentToolCall, ToolCall: &callCopy,
			})
		}
		transaction = append(
			transaction,
			provider.ProducedAssistant(spec.Route, blocks, e.turn, nil),
		)
		toolCtx := ctx
		if kernel.Convergence() != nil {
			toolCtx = tool.WithFinishOnly(ctx)
		}
		results, err := e.runToolsWithCache(
			toolCtx,
			turnID,
			recoveredCalls,
			executed,
			cache,
			kernel,
			send,
		)
		salvageCompletedResults(recoveredCalls)
		if canceled, cancelErr := finishAcceptedCancellation(); canceled {
			return result, cancelErr
		}
		if err != nil {
			return result, err
		}
		resultMessages, err := tool.ProjectModelResults(
			recoveredCalls,
			results,
			e.turn,
		)
		if err != nil {
			return result, err
		}
		transaction = append(transaction, resultMessages...)
		e.recordReadResults(recoveredCalls, results)
		persistContinuation("", 0)
	}
	// Tool-model usage is accounted separately from this route.
	var sampled provider.Usage
	var toolSpent toolSpend
	toolSpent.known = true
	gate := &verifyGate{
		engine: e,
		kernel: kernel,
	}
	sampleReason := promptcontext.SampleNormal
	convergenceFinalization := false
	invalidateCompletion := func(reason string) error {
		current := kernel.Completion()
		if current == nil || !current.Accepted {
			return nil
		}
		return kernel.InvalidateCompletion(reason)
	}
	completeTurn := func(outcome verifyOutcome) error {
		if outcome.receipt != nil {
			result.Verification = outcome.receipt
		}
		if err := kernel.ValidateFinalReadiness(); err != nil {
			return err
		}
		pricing := e.activeRoute().Model().Pricing
		cost := provider.EstimateCost(pricing, sampled) + toolSpent.cost
		costKnown := provider.PricingKnown(pricing, sampled) &&
			(toolSpent.samples == 0 || toolSpent.known)
		result.CostUSD = cost
		journalRevert = outcome.action == verifyActionReverted
		if e.journal == nil && journalRevert {
			return errors.New(
				"verification requested rollback without a workspace journal",
			)
		}
		if outcome.receipt != nil && outcome.receipt.Workspace == nil {
			outcome.receipt.Workspace = &VerificationWorkspace{Status: "changed"}
		}
		output, err := kernel.ReleaseOutput()
		if err != nil {
			return err
		}
		finalText := strings.Join(output, "")
		transaction = append(
			transaction,
			provider.ProducedAssistant(
				spec.Route,
				[]provider.ContentBlock{{
					Type: provider.ContentText,
					Text: finalText,
				}},
				e.turn,
				nil,
			),
		)
		result.Text, result.State = finalText, Completed
		snapshot, err := e.finalizeTerminalContext(
			transaction, true, false, nil, result.Usage, cost, send,
		)
		contextFinalized = true
		terminal.setContextBudget(snapshot)
		if err != nil {
			terminal.addSecondary("terminal_context", err)
		}
		if err := finalizeKernel(
			turnkernel.TerminalRequested{},
			nil,
		); err != nil {
			return err
		}
		if !journalRevert && e.journal != nil {
			e.turnIDs[turnID] = e.turn
		}
		if err := send(Completed, Event{
			Text: finalText, Usage: &result.Usage, CostUSD: cost,
			CostKnown: costKnown, Verification: outcome.receipt,
			Completion: kernel.CompletionDeclaration(),
			SecondaryIssues: append(
				[]TerminalIssue(nil),
				terminal.secondary...,
			),
		}); err != nil {
			return err
		}
		return nil
	}
	blockTurn := func() error {
		convergence := kernel.Convergence()
		if convergence == nil {
			return protocol.NewProblem(
				protocol.CodeInternal,
				"kernel requested blocked finalization without convergence state",
				false,
				nil,
			)
		}
		message := "turn declared incomplete with resumable pending actions"
		if convergence.Cause != turnkernel.ConvergenceIncomplete {
			message = fmt.Sprintf(
				"turn blocked after %s convergence budget was exhausted (%d/%d)",
				convergence.Cause,
				convergence.Used,
				convergence.Limit,
			)
		}
		blocked := protocol.NewProblem(
			protocol.CodeConflict,
			message,
			true,
			nil,
		)
		pricing := e.activeRoute().Model().Pricing
		cost := provider.EstimateCost(pricing, sampled) + toolSpent.cost
		costKnown := provider.PricingKnown(pricing, sampled) &&
			(toolSpent.samples == 0 || toolSpent.known)
		result.CostUSD = cost
		terminal.setPrimary(blocked)
		snapshot, err := e.finalizeTerminalContext(
			transaction, false, false, blocked, result.Usage, cost, send,
		)
		contextFinalized = true
		terminal.setContextBudget(snapshot)
		if err != nil {
			terminal.addSecondary("terminal_context", err)
		}
		if err := finalizeKernel(
			turnkernel.TerminalRequested{
				FailureCode:    string(protocol.CodeConflict),
				FailureMessage: message,
				Fault: protocol.CloneFaultMetadata(
					blocked.Fault,
				),
				Convergence: convergence,
			},
			nil,
		); err != nil {
			return errors.Join(blocked, err)
		}
		convergence = kernel.Convergence()
		result.State = Failed
		if err := send(Failed, Event{
			ErrorCode:    protocol.CodeConflict,
			Error:        message,
			Convergence:  turnkernel.ProtocolConvergence(convergence),
			Usage:        &result.Usage,
			CostUSD:      cost,
			CostKnown:    costKnown,
			Verification: result.Verification,
			Completion:   kernel.BlockedCompletionDeclaration(),
			SecondaryIssues: append(
				[]TerminalIssue(nil),
				terminal.secondary...,
			),
		}); err != nil {
			return errors.Join(blocked, err)
		}
		return blocked
	}
	advanceTurn := func() (bool, error) {
		var outcome verifyOutcome
		action, actionErr := kernel.EvaluateTurnStep(
			kernel.RepairProgressKey(),
		)
		if actionErr != nil {
			var exhausted *turnkernel.RepairBudgetExhaustedError
			if errors.As(actionErr, &exhausted) &&
				exhausted.Kind == turnkernel.RepairWorkspace {
				return false, protocol.NewProblem(
					protocol.CodeConflict,
					"workspace_change turn produced no observed workspace changes",
					false,
					actionErr,
				)
			}
			if errors.Is(actionErr, turnkernel.ErrRepairBudgetExhausted) {
				return false, protocol.NewProblem(
					protocol.CodeConflict,
					"turn repair made no progress",
					true,
					actionErr,
				)
			}
			return false, actionErr
		}
		switch action {
		case turnkernel.StepActionRepairToolFailure:
			if err := kernel.DiscardOutput("tool_failure_repair"); err != nil {
				return false, err
			}
			transaction = append(
				transaction,
				promptcontext.ToolFailureCompletionFeedback(e.turn),
			)
			sampleReason = promptcontext.SampleToolFailureRepair
			return false, nil
		case turnkernel.StepActionRepairCompletion:
			if err := kernel.DiscardOutput("completion_repair"); err != nil {
				return false, err
			}
			transaction = append(transaction, promptcontext.CompletionFeedback(e.turn))
			sampleReason = promptcontext.SampleCompletionRepair
			return false, nil
		case turnkernel.StepActionRepairWorkspace:
			if err := kernel.DiscardOutput("workspace_change_repair"); err != nil {
				return false, err
			}
			transaction = append(
				transaction,
				promptcontext.WorkspaceChangeRequiredFeedback(e.turn),
			)
			sampleReason = promptcontext.SampleWorkspaceRepair
			return false, nil
		case turnkernel.StepActionRepairDeclaration:
			// The narration that triggered this repair is the candidate
			// final answer: keep it so the follow-up declaration can seal
			// the captured body with output_mode=preserve_provisional
			// instead of rewriting it into the summary.
			transaction = append(
				transaction,
				promptcontext.CompletionDeclarationFeedback(e.turn),
			)
			sampleReason = promptcontext.SampleDeclarationRepair
			return false, nil
		case turnkernel.StepActionVerify:
			var err error
			outcome, err = gate.evaluate(ctx, send)
			if err != nil {
				return false, err
			}
			result.Verification = outcome.receipt
			switch outcome.action {
			case verifyActionRepair:
				if err := kernel.DiscardOutput("verification_repair"); err != nil {
					return false, err
				}
				transaction = append(
					transaction,
					verifyFeedback(outcome.receipt, e.turn),
				)
				sampleReason = promptcontext.SampleVerificationRepair
				return false, nil
			case verifyActionBlocked, verifyActionFailed:
				return false, protocol.NewProblem(
					protocol.CodeConflict,
					outcome.receipt.ProblemMessage(),
					false,
					nil,
				)
			}
		case turnkernel.StepActionFinalize:
			if err := kernel.BeginConvergenceFinalization(); err != nil {
				return false, err
			}
			transaction = append(
				transaction,
				promptcontext.ConvergenceFeedback(
					e.turn,
					string(kernel.Convergence().Cause),
					kernel.Convergence().Used,
					kernel.Convergence().Limit,
					string(kernel.Convergence().RepairKind),
					kernel.HasProvisionalOutput(),
				),
			)
			sampleReason = promptcontext.SampleConvergence
			convergenceFinalization = true
			return false, nil
		case turnkernel.StepActionBlock:
			return true, blockTurn()
		case turnkernel.StepActionComplete:
		default:
			return false, protocol.NewProblem(
				protocol.CodeInternal,
				fmt.Sprintf(
					"kernel returned unsupported step action %q",
					action,
				),
				false,
				nil,
			)
		}
		if err := completeTurn(outcome); err != nil {
			return false, err
		}
		return true, nil
	}
	lastSampleIdentity := ""
	for step := 0; ; step++ {
		if e.appendSteering(&transaction) && kernel.Completion() != nil {
			if err := invalidateCompletion("turn_steered"); err != nil {
				return result, err
			}
		}
		if completion := kernel.Completion(); completion != nil &&
			completion.Accepted {
			completed, err := advanceTurn()
			if err != nil {
				return result, err
			}
			if completed {
				return result, nil
			}
		}
		progressSignature := e.progressSignature(kernel)
		progress, err = kernel.ObserveProgress(
			progressSignature,
			lastSampleIdentity,
		)
		if err != nil {
			return result, err
		}
		if progress.StageChanged &&
			progress.Stage != turnkernel.ProgressStageNone {
			transaction = append(
				transaction,
				promptcontext.NoProgressFeedback(
					e.turn,
					progress.NoProgressSamples,
					string(progress.Stage),
					string(progress.StallKind),
				),
			)
		}
		if kernel.Convergence() != nil && !convergenceFinalization {
			completed, err := advanceTurn()
			if err != nil {
				return result, err
			}
			if completed {
				return result, nil
			}
			continue
		}
		sampleID := kernel.PendingSampleID()
		if sampleID == "" {
			sampleID = fmt.Sprintf("turn-%d-step-%d", e.turn, step+1)
			for kernel.HasSample(sampleID) {
				sampleID += "-recovered"
			}
		}
		if err := send(CallingModel, Event{
			ModelExecution: &ModelExecution{
				Kind: "model_sample", SampleID: sampleID,
				Reason: sampleReason,
			},
		}); err != nil {
			return result, err
		}
		assembly := kernel.SampleAssembly(sampleID)
		if assembly == nil {
			assembly = providerassembly.NewResponseAssembly(sampleID)
		}
		var modelOutputContinued bool
		var pendingInputInjected bool
		var modelReplay *provider.ReplayState
		modelSend := func(state State, event Event) error {
			if event.ProviderRetry != nil {
				if err := kernel.ScheduleProviderRetry(
					sampleID,
					kernelProviderRetry(*event.ProviderRetry),
				); err != nil {
					return err
				}
			}
			if state == Streaming &&
				event.Block != nil &&
				event.Block.Type == provider.ContentText {
				return nil
			}
			return send(state, event)
		}
		blocks, calls, usage, _, err := e.modelStep(
			ctx,
			&transaction,
			result.Usage,
			sampleID,
			sampleReason,
			kernel.ProviderRetries(sampleID),
			progress.Stage == turnkernel.ProgressStageFinishOnly &&
				turnkernel.IsResearchIntent(kernel.Intent()),
			convergenceFinalization,
			&modelOutputContinued,
			&pendingInputInjected,
			&modelReplay,
			assembly,
			func(current *providerassembly.ResponseAssembly) error {
				return kernel.RecordModelSampleProgress(
					sampleID,
					current,
				)
			},
			func() error {
				return kernel.BeginModelSample(ctx, sampleID)
			},
			func() error {
				return kernel.FinishModelTransport(sampleID)
			},
			modelSend,
		)
		convergenceFinalization = false
		sampleReason = promptcontext.SampleNormal
		sampleCost := provider.EstimateCost(spec.Route.Model().Pricing, usage)
		sampleCostKnown := provider.PricingKnown(
			spec.Route.Model().Pricing,
			usage,
		)
		if finishErr := kernel.FinishModelSample(
			sampleID,
			providerassembly.BlocksText(blocks),
			calls,
			usage,
			sampleCost,
			sampleCostKnown,
			modelOutputContinued,
			err,
			kernelProviderFailure(err),
		); finishErr != nil {
			return result, errors.Join(err, finishErr)
		}
		states := make([]turnkernel.ToolCallState, 0, len(calls))
		for _, call := range calls {
			states = append(states, turnkernel.ToolCallState{
				Name: call.Name, Arguments: call.Arguments,
			})
		}
		lastSampleIdentity = turnkernel.FormatToolCallsIdentity(states)
		if err == nil {
			terminal.publishCommentary(kernel.SampleCommentary(sampleID))
		}
		reasoning := providerassembly.BlocksReasoning(blocks)
		if strings.TrimSpace(reasoning) != "" {
			if sendErr := send(Streaming, Event{
				ReasoningCompleted: &ModelReasoning{
					SampleID: sampleID,
					Text:     reasoning,
				},
			}); sendErr != nil {
				return result, errors.Join(err, sendErr)
			}
		}
		if err != nil {
			return result, err
		}
		if pendingInputInjected && kernel.Completion() != nil {
			if err := invalidateCompletion("input_injected"); err != nil {
				return result, err
			}
		}
		result.Usage.Add(usage)
		sampled.Add(usage)
		result.Reasoning += reasoning
		for _, block := range blocks {
			if block.Type == provider.ContentSearch && block.Search != nil {
				result.Searches = append(result.Searches, *block.Search)
			}
			if block.Type == provider.ContentCitation && block.Citation != nil {
				result.Citations = append(result.Citations, *block.Citation)
			}
		}
		if len(calls) == 0 {
			if e.appendSteering(&transaction) {
				if kernel.Completion() != nil {
					if err := invalidateCompletion("turn_steered"); err != nil {
						return result, err
					}
				}
				if len(blocks) != 0 {
					transaction = append(
						transaction,
						provider.ProducedAssistant(
							spec.Route, blocks, e.turn, modelReplay,
						),
					)
				}
				persistContinuation(sampleID, step)
				continue
			}
			transaction = append(
				transaction,
				provider.ProducedAssistant(
					spec.Route, blocks, e.turn, modelReplay,
				),
			)
			persistContinuation(sampleID, step)
			completed, err := advanceTurn()
			if err != nil {
				return result, err
			}
			if completed {
				return result, nil
			}
			continue
		}
		if err := send(PreparingTools, Event{}); err != nil {
			return result, err
		}
		for _, call := range calls {
			callCopy := call
			blocks = append(blocks, provider.ContentBlock{Type: provider.ContentToolCall, ToolCall: &callCopy})
		}
		transaction = append(
			transaction,
			provider.ProducedAssistant(
				spec.Route, blocks, e.turn, modelReplay,
			),
		)
		toolCtx := ctx
		if kernel.Convergence() != nil {
			toolCtx = tool.WithFinishOnly(ctx)
		}
		results, err := e.runToolsWithCache(
			toolCtx,
			turnID,
			calls,
			executed,
			cache,
			kernel,
			send,
		)

		spend := e.drainToolSpend()
		result.Usage.Add(spend.usage)
		toolSpent.usage.Add(spend.usage)
		toolSpent.cost += spend.cost
		toolSpent.samples += spend.samples
		if spend.samples != 0 {
			toolSpent.known = toolSpent.known && spend.known
			if usageErr := kernel.RecordSupplementalUsage(
				"tool",
				fmt.Sprintf("tool-batch-%d", step),
				spend.usage,
				spend.cost,
				spend.known,
			); usageErr != nil {
				return result, errors.Join(err, usageErr)
			}
		}
		salvageCompletedResults(calls)
		if canceled, cancelErr := finishAcceptedCancellation(); canceled {
			return result, cancelErr
		}
		if err != nil {
			return result, err
		}
		result.Tools = append(result.Tools, calls...)
		if err := send(FeedingResults, Event{}); err != nil {
			return result, err
		}
		resultMessages, err := tool.ProjectModelResults(calls, results, e.turn)
		if err != nil {
			return result, err
		}
		transaction = append(transaction, resultMessages...)
		e.recordReadResults(calls, results)
		persistContinuation(sampleID, step)
		if completion := kernel.Completion(); completion != nil &&
			completion.Accepted {
			completed, err := advanceTurn()
			if err != nil {
				return result, err
			}
			if completed {
				return result, nil
			}
		}
	}
}

func errorText(err error) string {
	if err == nil {
		return "turn failed"
	}
	return err.Error()
}
