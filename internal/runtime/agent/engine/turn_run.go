package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	providerassembly "github.com/fwtllh-png/QCode/internal/adapter/provider/assembly"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/observability/verify"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	promptcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/prompt"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// turnRun is the mutable state of one Scope.Run after journal admission. The
// sampling loop, repair steps and terminal steps are methods over it, and
// settle is the one terminal step every exit path goes through.
//
// Terminal invariant: the SessionDelta staged for the terminal commit always
// describes the outcome the envelope emits. A terminal attempt stages the
// history for its outcome and then asks the kernel to finalize; if the kernel
// rejects the attempt the stage is discarded and settle restages the history
// of the failure that is emitted instead.
type turnRun struct {
	scope    *Scope
	e        *Engine
	spec     TurnSpec
	ctx      context.Context
	turnID   string
	kernel   *turnkernel.RuntimeKernel
	terminal *turnEmitter
	send     func(State, Event) error
	result   *Result

	transaction             []provider.Message
	executed                map[string]tool.Result
	cache                   *toolResultCache
	gate                    *verifyGate
	sampled                 provider.Usage
	toolSpent               toolSpend
	progress                turnkernel.ProgressObservation
	sampleReason            string
	convergenceFinalization bool
	lastSampleIdentity      string

	contextStaged   bool
	kernelStarted   bool
	kernelFinalized bool
}

func newTurnRun(
	ctx context.Context,
	scope *Scope,
	kernel *turnkernel.RuntimeKernel,
	terminal *turnEmitter,
	transaction []provider.Message,
	result *Result,
) *turnRun {
	run := &turnRun{
		scope:        scope,
		e:            scope.engine,
		spec:         scope.spec,
		ctx:          ctx,
		turnID:       scope.spec.Identity.TurnID,
		kernel:       kernel,
		terminal:     terminal,
		send:         terminal.send,
		result:       result,
		transaction:  transaction,
		executed:     make(map[string]tool.Result),
		cache:        &toolResultCache{},
		gate:         &verifyGate{engine: scope.engine, kernel: kernel},
		sampleReason: promptcontext.SampleNormal,
	}
	run.toolSpent.known = true
	return run
}

// execute drives the turn from its opening phase to a terminal step. A nil
// return means a terminal envelope was emitted; any error is settled.
func (r *turnRun) execute(
	continuation agentcontext.TurnContinuation,
	continuationUsable bool,
) error {
	for _, message := range r.kernel.CommentaryMessages() {
		r.terminal.publishCommentary(&message)
	}
	if done, err := r.resumeTerminal(); done {
		return err
	}
	if r.kernel.CancellationReason() != "" {
		return context.Canceled
	}
	if err := r.send(Preparing, Event{
		Provider: r.spec.Provider, Model: r.spec.Model,
		ModelMetadata:   r.spec.ModelMetadata,
		Purpose:         string(r.spec.Purpose),
		ProfileRevision: r.spec.Identity.ProfileRevision,
		Mode:            string(r.spec.Mode), Posture: string(r.spec.Posture),
		Workspace:          r.spec.Workspace,
		WorkspaceIsolation: r.e.options.WorkspaceIsolation,
		Sandbox:            r.spec.Sandbox,
	}); err != nil {
		return err
	}
	r.openConversation(continuation, continuationUsable)
	r.progress = r.kernel.ProgressObservation()
	if done, err := r.resumePendingTools(); done || err != nil {
		return err
	}
	for step := 0; ; step++ {
		if done, err := r.sampleStep(step); done || err != nil {
			return err
		}
	}
}

// resumeTerminal re-emits a terminal decision the kernel already owns: a
// committing decision still has journal effects to finish, a terminal one only
// needs its envelope.
func (r *turnRun) resumeTerminal() (bool, error) {
	if decision, resuming := r.kernel.CommittingDecision(); resuming {
		r.kernelStarted = true
		r.contextStaged = true
		r.terminal.setContextBudget(ContextBudgetSnapshot{})
		if err := r.finalizeKernel(turnkernel.TerminalRequested{}, &decision); err != nil {
			return true, err
		}
		return true, r.emitDecision(decision, false)
	}
	if decision, terminalized := r.kernel.TerminalDecision(); terminalized {
		r.kernelStarted = true
		r.kernelFinalized = true
		r.contextStaged = true
		r.terminal.setContextBudget(ContextBudgetSnapshot{})
		return true, r.emitDecision(decision, true)
	}
	return false, nil
}

func (r *turnRun) emitDecision(
	decision turnkernel.TerminalDecision,
	withCode bool,
) error {
	switch decision.Kind {
	case turnkernel.TerminalCompleted:
		r.result.Text = r.kernel.FrozenOutput()
		r.result.State = Completed
		return r.send(Completed, Event{Text: r.result.Text})
	case turnkernel.TerminalCanceled:
		r.result.State = Canceled
		return r.send(Canceled, Event{CancelReason: decision.Message})
	default:
		r.result.State = Failed
		event := Event{Error: decision.Message}
		if withCode {
			event.ErrorCode = protocol.ErrorCode(decision.Code)
		}
		return r.send(Failed, event)
	}
}

func (r *turnRun) openConversation(
	continuation agentcontext.TurnContinuation,
	continuationUsable bool,
) {
	if continuationUsable {
		r.scope.mu.Lock()
		r.scope.state.referenceRecoveryOnly = continuation.ReferenceRecoveryOnly
		r.scope.mu.Unlock()
		if continuation.ContextCaptured {
			r.e.contextAuthority().SetConversation(continuation.Conversation)
			plan := agentcontext.Plan{}
			if continuation.Plan != nil {
				plan = continuation.Plan.Clone()
			}
			r.e.setPlan(plan)
		}
		// A restored Turn's accepted conversation already contains its
		// opening user request; re-appending the submitted prompt would
		// duplicate the goal the continuation carries.
		r.transaction = append(r.transaction, continuation.Messages...)
		return
	}
	user := provider.TextMessage(provider.RoleUser, r.spec.Request.Prompt)
	for index := range r.spec.Request.Attachments {
		attachment := r.spec.Request.Attachments[index]
		user.Blocks = append(user.Blocks, provider.ContentBlock{
			Type: provider.ContentImage, Attachment: &attachment,
		})
	}
	user.Turn = r.e.turn
	r.transaction = append(r.transaction, user)
}

// resumePendingTools closes the tool calls a restored kernel still owns
// before the first new sample.
func (r *turnRun) resumePendingTools() (bool, error) {
	recoveredCalls := r.kernel.PendingToolCalls()
	if len(recoveredCalls) == 0 {
		return false, nil
	}
	blocks := make([]provider.ContentBlock, 0, len(recoveredCalls))
	for _, call := range recoveredCalls {
		callCopy := call
		blocks = append(blocks, provider.ContentBlock{
			Type: provider.ContentToolCall, ToolCall: &callCopy,
		})
	}
	r.transaction = append(
		r.transaction,
		provider.ProducedAssistant(r.spec.Route, blocks, r.e.turn, nil),
	)
	results, err := r.runTools(recoveredCalls)
	r.salvageCompletedResults(recoveredCalls)
	if canceled, cancelErr := r.finishAcceptedCancellation(); canceled {
		return true, cancelErr
	}
	if err != nil {
		return true, err
	}
	resultMessages, err := tool.ProjectModelResults(recoveredCalls, results, r.e.turn)
	if err != nil {
		return true, err
	}
	r.transaction = append(r.transaction, resultMessages...)
	r.e.recordReadResults(recoveredCalls, results)
	r.persistContinuation("", 0)
	return false, nil
}

func (r *turnRun) runTools(calls []provider.ToolCall) ([]tool.Result, error) {
	toolCtx := r.ctx
	if r.kernel.Convergence() != nil {
		toolCtx = tool.WithFinishOnly(r.ctx)
	}
	return r.e.runToolsWithCache(
		toolCtx,
		r.turnID,
		calls,
		r.executed,
		r.cache,
		r.kernel,
		r.send,
	)
}

// sampleStep runs one model sample and the phase it leads to. It reports
// done once a terminal envelope was emitted; an error ends the turn.
func (r *turnRun) sampleStep(step int) (bool, error) {
	e, kernel := r.e, r.kernel
	if e.appendSteering(&r.transaction) && kernel.Completion() != nil {
		if err := r.invalidateCompletion("turn_steered"); err != nil {
			return true, err
		}
	}
	if completion := kernel.Completion(); completion != nil && completion.Accepted {
		if completed, err := r.advanceTurn(); completed || err != nil {
			return true, err
		}
	}
	var err error
	r.progress, err = kernel.ObserveProgress(
		e.progressSignature(kernel),
		r.lastSampleIdentity,
	)
	if err != nil {
		return true, err
	}
	if r.progress.StageChanged &&
		r.progress.Stage != turnkernel.ProgressStageNone {
		r.transaction = append(
			r.transaction,
			promptcontext.NoProgressFeedback(
				e.turn,
				r.progress.NoProgressSamples,
				string(r.progress.Stage),
				string(r.progress.StallKind),
			),
		)
	}
	if kernel.Convergence() != nil && !r.convergenceFinalization {
		completed, err := r.advanceTurn()
		return completed || err != nil, err
	}
	sampleID := kernel.PendingSampleID()
	if sampleID == "" {
		sampleID = fmt.Sprintf("turn-%d-step-%d", e.turn, step+1)
		for kernel.HasSample(sampleID) {
			sampleID += "-recovered"
		}
	}
	if err := r.send(CallingModel, Event{
		ModelExecution: &ModelExecution{
			Kind: "model_sample", SampleID: sampleID,
			Reason: r.sampleReason,
		},
	}); err != nil {
		return true, err
	}
	assembly := kernel.SampleAssembly(sampleID)
	if assembly == nil {
		assembly = providerassembly.NewResponseAssembly(sampleID)
	}
	var modelOutputContinued bool
	var pendingInputInjected bool
	var modelReplay *provider.ReplayState
	// Body text deltas are streamed optimistically: whether a sample's text
	// is the final answer or narration that accompanies tool calls is only
	// known when the sample closes, so retractions are signalled with
	// OutputDiscarded instead of withholding the stream.
	var streamedSampleText bool
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
			streamedSampleText = true
		}
		return r.send(state, event)
	}
	blocks, calls, usage, _, sampleErr := e.modelStep(
		r.ctx,
		&r.transaction,
		r.result.Usage,
		sampleID,
		r.sampleReason,
		modelRetryState{
			budget: kernel.ProviderRetryBudget(sampleID),
			reserveWait: func(delay time.Duration, until time.Time) error {
				return kernel.ReserveProviderWait(sampleID, delay, until)
			},
		},
		r.progress.Stage == turnkernel.ProgressStageFinishOnly &&
			turnkernel.IsResearchIntent(kernel.Intent()),
		r.convergenceFinalization,
		&modelOutputContinued,
		&pendingInputInjected,
		&modelReplay,
		assembly,
		func(current *providerassembly.ResponseAssembly) error {
			return kernel.RecordModelSampleProgress(sampleID, current)
		},
		func() error {
			return kernel.BeginModelSample(r.ctx, sampleID)
		},
		func() error {
			return kernel.FinishModelTransport(sampleID)
		},
		modelSend,
	)
	r.convergenceFinalization = false
	r.sampleReason = promptcontext.SampleNormal
	sampleCost := provider.EstimateCost(r.spec.Route.Model().Pricing, usage)
	sampleCostKnown := provider.PricingKnown(r.spec.Route.Model().Pricing, usage)
	if finishErr := kernel.FinishModelSample(
		sampleID,
		providerassembly.BlocksText(blocks),
		calls,
		usage,
		sampleCost,
		sampleCostKnown,
		modelOutputContinued,
		sampleErr,
		kernelProviderFailure(sampleErr),
	); finishErr != nil {
		return true, errors.Join(sampleErr, finishErr)
	}
	states := make([]turnkernel.ToolCallState, 0, len(calls))
	for _, call := range calls {
		states = append(states, turnkernel.ToolCallState{
			Name: call.Name, Arguments: call.Arguments,
		})
	}
	r.lastSampleIdentity = turnkernel.FormatToolCallsIdentity(states)
	if sampleErr == nil {
		r.terminal.publishCommentary(kernel.SampleCommentary(sampleID))
	}
	reasoning := providerassembly.BlocksReasoning(blocks)
	if strings.TrimSpace(reasoning) != "" {
		if sendErr := r.send(Streaming, Event{
			ReasoningCompleted: &ModelReasoning{
				SampleID: sampleID,
				Text:     reasoning,
			},
		}); sendErr != nil {
			return true, errors.Join(sampleErr, sendErr)
		}
	}
	if sampleErr != nil {
		return true, sampleErr
	}
	if pendingInputInjected && kernel.Completion() != nil {
		if err := r.invalidateCompletion("input_injected"); err != nil {
			return true, err
		}
	}
	r.result.Usage.Add(usage)
	r.sampled.Add(usage)
	r.result.Reasoning += reasoning
	for _, block := range blocks {
		if block.Type == provider.ContentSearch && block.Search != nil {
			r.result.Searches = append(r.result.Searches, *block.Search)
		}
		if block.Type == provider.ContentCitation && block.Citation != nil {
			r.result.Citations = append(r.result.Citations, *block.Citation)
		}
	}
	if len(calls) == 0 {
		return r.closeAnswerSample(sampleID, step, blocks, modelReplay)
	}
	// The sample's text accompanied its tool calls, so it is narration:
	// the commentary projection republishes it durably. Retract the
	// provisional deltas before the tool phase replaces the sample.
	if streamedSampleText {
		if err := r.send(Streaming, Event{
			OutputDiscarded: &ModelOutputDiscarded{
				SampleID: sampleID,
				Reason:   "narration",
			},
		}); err != nil {
			return true, err
		}
	}
	return r.runToolPhase(sampleID, step, blocks, calls, modelReplay)
}

// closeAnswerSample records a sample without tool calls. Input that arrived
// after the model handler's own drain is a reaction to this sample, so it is
// appended after the sample's output and the turn samples again.
func (r *turnRun) closeAnswerSample(
	sampleID string,
	step int,
	blocks []provider.ContentBlock,
	replay *provider.ReplayState,
) (bool, error) {
	pending := r.e.drainPending()
	if len(pending) == 0 || len(blocks) != 0 {
		r.transaction = append(
			r.transaction,
			provider.ProducedAssistant(r.spec.Route, blocks, r.e.turn, replay),
		)
	}
	if len(pending) != 0 {
		r.e.appendPendingInputs(&r.transaction, pending)
		if r.kernel.Completion() != nil {
			if err := r.invalidateCompletion("turn_steered"); err != nil {
				return true, err
			}
		}
		r.persistContinuation(sampleID, step)
		return false, nil
	}
	r.persistContinuation(sampleID, step)
	completed, err := r.advanceTurn()
	return completed || err != nil, err
}

func (r *turnRun) runToolPhase(
	sampleID string,
	step int,
	blocks []provider.ContentBlock,
	calls []provider.ToolCall,
	replay *provider.ReplayState,
) (bool, error) {
	e, kernel := r.e, r.kernel
	if err := r.send(PreparingTools, Event{}); err != nil {
		return true, err
	}
	for _, call := range calls {
		callCopy := call
		blocks = append(blocks, provider.ContentBlock{Type: provider.ContentToolCall, ToolCall: &callCopy})
	}
	r.transaction = append(
		r.transaction,
		provider.ProducedAssistant(r.spec.Route, blocks, e.turn, replay),
	)
	results, err := r.runTools(calls)
	spend := e.drainToolSpend()
	r.result.Usage.Add(spend.usage)
	r.toolSpent.usage.Add(spend.usage)
	r.toolSpent.cost += spend.cost
	r.toolSpent.samples += spend.samples
	if spend.samples != 0 {
		r.toolSpent.known = r.toolSpent.known && spend.known
		if usageErr := kernel.RecordSupplementalUsage(
			"tool",
			fmt.Sprintf("tool-batch-%d", step),
			spend.usage,
			spend.cost,
			spend.known,
		); usageErr != nil {
			return true, errors.Join(err, usageErr)
		}
	}
	r.salvageCompletedResults(calls)
	if canceled, cancelErr := r.finishAcceptedCancellation(); canceled {
		return true, cancelErr
	}
	if err != nil {
		return true, err
	}
	r.result.Tools = append(r.result.Tools, calls...)
	if err := r.send(FeedingResults, Event{}); err != nil {
		return true, err
	}
	resultMessages, err := tool.ProjectModelResults(calls, results, e.turn)
	if err != nil {
		return true, err
	}
	r.transaction = append(r.transaction, resultMessages...)
	e.recordReadResults(calls, results)
	r.persistContinuation(sampleID, step)
	if completion := kernel.Completion(); completion != nil && completion.Accepted {
		completed, err := r.advanceTurn()
		return completed || err != nil, err
	}
	return false, nil
}

// salvageCompletedResults projects the durably closed calls of an
// interrupted batch into the transaction. Calls without a kernel-closed
// result stay dangling and are dropped by pair normalization during
// settlement, so the model never sees an unclosed tool pair but the
// completed evidence survives the cancellation.
func (r *turnRun) salvageCompletedResults(calls []provider.ToolCall) {
	if r.kernel.CancellationReason() == "" {
		return
	}
	completedCalls := make([]provider.ToolCall, 0, len(calls))
	completedResults := make([]tool.Result, 0, len(calls))
	for _, call := range calls {
		if result, ok := r.executed[call.ID]; ok {
			completedCalls = append(completedCalls, call)
			completedResults = append(completedResults, result)
		}
	}
	if len(completedCalls) == 0 {
		return
	}
	messages, err := tool.ProjectModelResults(completedCalls, completedResults, r.e.turn)
	if err != nil {
		return
	}
	r.transaction = append(r.transaction, messages...)
}

// persistContinuation stores the accepted in-turn conversation at
// semantic boundaries. The content is written before the kernel commits
// the cursor, so a committed continuation fact always resolves and a
// crash between the two leaves only an unreferenced blob for storage
// governance to collect.
func (r *turnRun) persistContinuation(sampleID string, step int) {
	e := r.e
	blobs := e.options.TurnContinuations
	if blobs == nil || !e.continuationEnvironmentComplete(r.spec) {
		return
	}
	messages, _, err := agentcontext.NormalizePairs(
		currentTurnMessages(r.transaction, e.turn),
	)
	if err != nil {
		r.terminal.addSecondary("turn_continuation", err)
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
		Version:               agentcontext.ContinuationVersion,
		TurnID:                r.turnID,
		Sequence:              r.kernel.NextContinuationSequence(),
		TurnNumber:            e.turn,
		SessionRevision:       e.sessionRevision,
		StateEpoch:            max(uint64(1), e.stateEpoch),
		SampleID:              sampleID,
		Step:                  step,
		WorkspaceIdentity:     e.options.WorkspaceIdentity,
		ProfileRevision:       r.spec.Identity.ProfileRevision,
		Provider:              r.spec.Provider,
		Model:                 r.spec.Model,
		Messages:              messages,
		ContextCaptured:       true,
		ReferenceRecoveryOnly: e.referenceRecoveryOnly(),
		Conversation:          e.contextAuthority().Conversation(),
	}
	plan := e.currentPlan()
	record.Plan = &plan
	stageCtx, finishStage := agentcontext.BeginContentStage(r.ctx, blobs)
	defer func() {
		if err := finishStage(); err != nil {
			r.terminal.addSecondary("turn_continuation", err)
		}
	}()
	ref, err := agentcontext.StoreTurnContinuation(stageCtx, blobs, record)
	if err != nil {
		r.terminal.addSecondary("turn_continuation", err)
		return
	}
	if err := r.kernel.RecordContinuation(turnkernel.ContinuationCursor{
		Sequence: record.Sequence,
		Handle:   ref.Handle,
		Digest:   ref.Digest,
	}); err != nil {
		r.terminal.addSecondary("turn_continuation", err)
	}
}

func (r *turnRun) invalidateCompletion(reason string) error {
	current := r.kernel.Completion()
	if current == nil || !current.Accepted {
		return nil
	}
	return r.kernel.InvalidateCompletion(reason)
}

// advanceTurn asks the kernel what the accepted completion leads to: a
// repair sample, verification, convergence finalization, or a terminal step.
func (r *turnRun) advanceTurn() (bool, error) {
	e, kernel := r.e, r.kernel
	if err := e.reconcileWorkspace(kernel); err != nil {
		return false, err
	}
	var outcome verifyOutcome
	action, actionErr := kernel.EvaluateTurnStep(kernel.RepairProgressKey())
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
		r.transaction = append(r.transaction, promptcontext.ToolFailureCompletionFeedback(e.turn))
		r.sampleReason = promptcontext.SampleToolFailureRepair
		return false, nil
	case turnkernel.StepActionRepairCompletion:
		if err := kernel.DiscardOutput("completion_repair"); err != nil {
			return false, err
		}
		r.transaction = append(r.transaction, promptcontext.CompletionFeedback(e.turn))
		r.sampleReason = promptcontext.SampleCompletionRepair
		return false, nil
	case turnkernel.StepActionRepairWorkspace:
		if err := kernel.DiscardOutput("workspace_change_repair"); err != nil {
			return false, err
		}
		r.transaction = append(r.transaction, promptcontext.WorkspaceChangeRequiredFeedback(e.turn))
		r.sampleReason = promptcontext.SampleWorkspaceRepair
		return false, nil
	case turnkernel.StepActionRepairDeclaration:
		// The narration that triggered this repair is the candidate
		// final answer: keep it so the follow-up declaration can seal
		// the captured body with output_mode=preserve_provisional
		// instead of rewriting it into the summary.
		r.transaction = append(r.transaction, promptcontext.CompletionDeclarationFeedback(e.turn))
		r.sampleReason = promptcontext.SampleDeclarationRepair
		return false, nil
	case turnkernel.StepActionVerify:
		var err error
		outcome, err = r.gate.evaluate(r.ctx, r.send)
		if err != nil {
			return false, err
		}
		r.result.Verification = outcome.receipt
		switch outcome.action {
		case verifyActionRepair:
			if err := kernel.DiscardOutput("verification_repair"); err != nil {
				return false, err
			}
			r.transaction = append(r.transaction, verifyFeedback(outcome.receipt, e.turn))
			r.sampleReason = promptcontext.SampleVerificationRepair
			return false, nil
		case verifyActionBlocked:
			return false, verificationFailure(outcome.receipt, true)
		case verifyActionFailed:
			return false, verificationFailure(outcome.receipt, false)
		}
	case turnkernel.StepActionFinalize:
		if err := kernel.BeginConvergenceFinalization(); err != nil {
			return false, err
		}
		convergence := kernel.Convergence()
		r.transaction = append(
			r.transaction,
			promptcontext.ConvergenceFeedback(
				e.turn,
				string(convergence.Cause),
				convergence.Used,
				convergence.Limit,
				string(convergence.RepairKind),
				kernel.HasProvisionalOutput(),
			),
		)
		r.sampleReason = promptcontext.SampleConvergence
		r.convergenceFinalization = true
		return false, nil
	case turnkernel.StepActionBlock:
		return true, r.blockTurn()
	case turnkernel.StepActionComplete:
	default:
		return false, protocol.NewProblem(
			protocol.CodeInternal,
			fmt.Sprintf("kernel returned unsupported step action %q", action),
			false,
			nil,
		)
	}
	return r.completeTurn(outcome)
}

func (r *turnRun) cost() (float64, bool) {
	pricing := r.e.activeRoute().Model().Pricing
	cost := provider.EstimateCost(pricing, r.sampled) + r.toolSpent.cost
	known := provider.PricingKnown(pricing, r.sampled) &&
		(r.toolSpent.samples == 0 || r.toolSpent.known)
	return cost, known
}

// completeTurn is the completion terminal step. It first closes steering: a
// steer that is still pending continues the turn instead of being dropped by
// a completion that never showed it to the model.
func (r *turnRun) completeTurn(outcome verifyOutcome) (bool, error) {
	e, kernel := r.e, r.kernel
	if r.scope.closeSteering(true) {
		e.appendSteering(&r.transaction)
		return false, r.invalidateCompletion("turn_steered")
	}
	if outcome.receipt != nil {
		r.result.Verification = outcome.receipt
	}
	if err := kernel.ValidateFinalReadiness(); err != nil {
		return false, err
	}
	cost, costKnown := r.cost()
	r.result.CostUSD = cost
	journalRevert := outcome.action == verifyActionReverted
	if e.journal == nil && journalRevert {
		return false, errors.New(
			"verification requested rollback without a workspace journal",
		)
	}
	if outcome.receipt != nil && outcome.receipt.Workspace == nil {
		outcome.receipt.Workspace = &VerificationWorkspace{Status: "changed"}
	}
	output, err := kernel.ReleaseOutput()
	if err != nil {
		return false, err
	}
	finalText := strings.Join(output, "")
	r.transaction = append(
		r.transaction,
		provider.ProducedAssistant(
			r.spec.Route,
			[]provider.ContentBlock{{Type: provider.ContentText, Text: finalText}},
			e.turn,
			nil,
		),
	)
	r.result.Text = finalText
	_ = r.stageContext(true, "", nil, r.result.Usage, cost)
	if err := r.finalizeStaged(turnkernel.TerminalRequested{}); err != nil {
		return false, err
	}
	r.result.State = Completed
	if !journalRevert && e.journal != nil {
		e.turnIDs[r.turnID] = e.turn
	}
	if err := r.send(Completed, Event{
		Text: finalText, Usage: &r.result.Usage, CostUSD: cost,
		CostKnown: costKnown, Verification: outcome.receipt,
		Completion:      kernel.CompletionDeclaration(),
		SecondaryIssues: append([]TerminalIssue(nil), r.terminal.secondary...),
	}); err != nil {
		return false, err
	}
	return true, nil
}

// blockTurn is the terminal step for a turn the kernel declared blocked
// after convergence; it fails with resumable pending actions.
func (r *turnRun) blockTurn() error {
	kernel := r.kernel
	r.scope.closeSteering(false)
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
	blocked := protocol.NewProblem(protocol.CodeConflict, message, true, nil)
	cost, costKnown := r.cost()
	r.result.CostUSD = cost
	r.terminal.setPrimary(blocked)
	_ = r.stageContext(false, "", blocked, r.result.Usage, cost)
	if err := r.finalizeStaged(turnkernel.TerminalRequested{
		FailureCode:    string(protocol.CodeConflict),
		FailureMessage: message,
		Fault:          protocol.CloneFaultMetadata(blocked.Fault),
		Convergence:    convergence,
	}); err != nil {
		return errors.Join(blocked, err)
	}
	convergence = kernel.Convergence()
	r.result.State = Failed
	if err := r.send(Failed, Event{
		ErrorCode:       protocol.CodeConflict,
		Error:           message,
		Convergence:     turnkernel.ProtocolConvergence(convergence),
		Usage:           &r.result.Usage,
		CostUSD:         cost,
		CostKnown:       costKnown,
		Verification:    r.result.Verification,
		Completion:      kernel.BlockedCompletionDeclaration(),
		SecondaryIssues: append([]TerminalIssue(nil), r.terminal.secondary...),
	}); err != nil {
		return errors.Join(blocked, err)
	}
	return blocked
}

// finishAcceptedCancellation is the terminal step for a cancel the kernel
// accepted while tools ran. The settlement runs before the kernel finalizes
// so the terminal commit carries the closed conversation: published but
// unprojected tool results stay recoverable instead of vanishing with the
// canceled batch.
func (r *turnRun) finishAcceptedCancellation() (bool, error) {
	reason := r.kernel.CancellationReason()
	if reason == "" {
		return false, nil
	}
	r.scope.closeSteering(false)
	_ = r.stageContext(false, reason, nil, provider.Usage{}, 0)
	if err := r.finalizeStaged(turnkernel.TerminalRequested{CancelReason: reason}); err != nil {
		return true, err
	}
	r.result.State = Canceled
	return true, r.send(Canceled, Event{CancelReason: reason})
}

// stageContext stages the SessionDelta for one terminal outcome. A canceled
// outcome keeps the turn's closed exchanges only when its journal keeps the
// draft; otherwise the workspace is rolled back and so is the conversation.
func (r *turnRun) stageContext(
	completed bool,
	cancelReason string,
	failure error,
	usage provider.Usage,
	cost float64,
) error {
	canceled := cancelReason != "" && turnkernel.CancelSuspendsDraft(cancelReason)
	snapshot, err := r.e.finalizeTerminalContext(
		r.transaction, completed, canceled, failure, usage, cost, r.send,
	)
	r.contextStaged = true
	r.terminal.setContextBudget(snapshot)
	if err != nil {
		r.terminal.addSecondary("terminal_context", err)
	}
	return err
}

// finalizeStaged finalizes the kernel for the outcome whose context is
// staged. A rejected attempt discards the stage so settle restages the
// history of the failure that is emitted instead.
func (r *turnRun) finalizeStaged(request turnkernel.TerminalRequested) error {
	err := r.finalizeKernel(request, nil)
	if err != nil && !r.terminal.recoveryPending {
		r.e.discardSessionDelta()
		r.contextStaged = false
	}
	return err
}

func (r *turnRun) finalizeKernel(
	request turnkernel.TerminalRequested,
	resumed *turnkernel.TerminalDecision,
) error {
	if r.kernelFinalized {
		return nil
	}
	e := r.e
	journal := turnkernel.JournalDriver{}
	if e.journal != nil {
		journal.Commit = func() error {
			return e.journal.Commit(r.turnID)
		}
		journal.Suspend = func() error {
			err := e.journal.Suspend(r.turnID)
			if r.result.Verification != nil {
				r.result.Verification.Workspace = &VerificationWorkspace{
					Status: "draft",
					Note:   "workspace changes are retained as a resumable, unverified draft",
				}
			}
			return err
		}
		journal.Rollback = func() error {
			receipt, err := e.journal.Rollback(context.Background(), r.turnID)
			if r.result.Verification != nil {
				r.result.Verification.Workspace = verify.WorkspaceFromJournal(receipt)
			}
			e.recordRollbackConflicts(receipt)
			return err
		}
	}
	finalized, err := r.kernel.FinalizeTerminal(request, resumed, journal)
	r.kernelStarted = r.kernelStarted || finalized.Started
	r.kernelFinalized = finalized.Finalized
	if err != nil {
		return err
	}
	if finalized.Pending != nil {
		r.terminal.suspendForRecovery()
		r.result.State = AwaitingRecovery
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
		projectionErr := r.send(AwaitingRecovery, Event{
			ErrorCode: fault.Code,
			Error:     fault.Message,
			Fault:     fault.Fault,
		})
		return errors.Join(fault, projectionErr)
	}
	return nil
}

// settle is the terminal step for every exit of Run that has not emitted a
// terminal envelope. The order is the terminal invariant: decide the outcome
// once, stage its history, finalize the kernel for it, then emit it.
func (r *turnRun) settle(resultErr *error) {
	r.scope.closeSteering(false)
	if r.terminal.emitted || r.terminal.recoveryPending {
		return
	}
	_, _, request := r.terminal.terminalRequest(r.ctx, *resultErr)
	if !r.contextStaged {
		var failure error
		if request.CancelReason == "" {
			failure = *resultErr
		}
		if err := r.stageContext(false, request.CancelReason, failure, provider.Usage{}, 0); err != nil {
			*resultErr = errors.Join(*resultErr, err)
		}
	}
	if !r.kernelStarted {
		if err := r.finalizeKernel(request, nil); err != nil {
			r.terminal.addSecondary("journal", err)
			*resultErr = errors.Join(*resultErr, err)
		}
	}
	r.terminal.finishTerminal(r.ctx, r.result, resultErr)
}
