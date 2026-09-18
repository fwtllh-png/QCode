package turnkernel

import (
	"errors"
	"fmt"
	"strings"

	providerassembly "github.com/fwtllh-png/QCode/internal/adapter/provider/assembly"
)

func applyModelSampleRequested(
	transition *Transition,
	current State,
	command ModelSampleRequested,
) error {
	if err := requirePhase(current, command, PhaseSampling); err != nil {
		return err
	}
	if current.Cancellation.Accepted {
		return illegal(current, command, "cancellation was accepted")
	}
	if strings.TrimSpace(command.SampleID) == "" {
		return illegal(current, command, "sample identity is empty")
	}
	if current.ActiveSampleID != "" {
		return illegal(current, command, "another model sample is active")
	}
	if _, exists := current.SampleLedger[command.SampleID]; exists {
		return illegal(current, command, "sample identity is duplicated")
	}
	transition.State.SampleLedger[command.SampleID] = ModelSampleState{
		ID:     command.SampleID,
		Status: SampleRequested,
	}
	transition.State.NextAction = StepActionNone
	requestEffect(
		transition,
		EffectSampleProvider,
		command,
		"sample:"+command.SampleID,
		command.SampleID,
	)
	return nil
}

func applyModelSampleStarted(
	transition *Transition,
	current State,
	command ModelSampleStarted,
) error {
	if err := requirePhase(current, command, PhaseSampling); err != nil {
		return err
	}
	if current.Cancellation.Accepted {
		return illegal(current, command, "cancellation was accepted")
	}
	if strings.TrimSpace(command.SampleID) == "" || command.Attempt == 0 {
		return illegal(current, command, "sample identity is incomplete")
	}
	if current.ActiveSampleID != "" {
		return illegal(current, command, "another model sample is active")
	}
	if existing, ok := current.SampleLedger[command.SampleID]; ok &&
		command.Attempt <= existing.Attempt {
		return illegal(current, command, "sample attempt is not monotonic")
	}
	transition.State.SampleLedger[command.SampleID] = ModelSampleState{
		ID:      command.SampleID,
		Attempt: command.Attempt,
		Status:  SampleRunning,
	}
	transition.State.ActiveSampleID = command.SampleID
	transition.Events = append(transition.Events, Event{
		Kind: EventSampleStarted, SampleID: command.SampleID,
	})
	return nil
}

func applyModelSampleFinished(
	transition *Transition,
	current State,
	command ModelSampleFinished,
) error {
	if err := requirePhase(current, command, PhaseSampling); err != nil {
		return err
	}
	if current.ActiveSampleID == "" ||
		current.ActiveSampleID != command.SampleID {
		return illegal(current, command, "sample result does not match active sample")
	}
	sample := current.SampleLedger[command.SampleID]
	sample.Status = SampleCompleted
	sample.Error = ""
	if command.Error != "" {
		sample.Status = SampleFailed
		sample.Error = command.Error
	}
	transition.State.SampleLedger[command.SampleID] = sample
	transition.State.ActiveSampleID = ""
	mergeUsage(&transition.State.Usage, command.Usage)
	transition.State.Context = command.Context
	transition.State.Context.Frozen = false
	transition.Events = append(transition.Events, Event{
		Kind: EventSampleFinished, SampleID: command.SampleID,
	})
	return nil
}

func applyModelSampleProgress(
	transition *Transition,
	current State,
	command ModelSampleProgressRecorded,
) error {
	if err := requirePhase(current, command, PhaseSampling); err != nil {
		return err
	}
	effect, ok := current.PendingEffects[command.EffectID]
	sample, sampleOK := current.SampleLedger[command.SampleID]
	if strings.TrimSpace(command.EffectID) == "" ||
		strings.TrimSpace(command.SampleID) == "" ||
		command.Attempt == 0 ||
		!ok ||
		effect.Kind != EffectSampleProvider ||
		effect.CallID != command.SampleID ||
		effect.Status != EffectRunning ||
		effect.Attempt != command.Attempt ||
		!sampleOK ||
		sample.Status != SampleRunning ||
		sample.Attempt != command.Attempt ||
		current.ActiveSampleID != command.SampleID {
		return illegal(
			current,
			command,
			"sample progress does not match the running provider effect",
		)
	}
	if command.Assembly.LogicalRequestID != command.SampleID {
		return illegal(
			current,
			command,
			"sample progress changed logical request identity",
		)
	}
	if err := command.Assembly.ValidateExtension(sample.Assembly); err != nil {
		return illegal(current, command, err.Error())
	}
	sample.Assembly = providerassembly.CloneResponseAssembly(
		&command.Assembly,
	)
	transition.State.SampleLedger[command.SampleID] = sample
	return nil
}

func applyModelSampleResult(
	transition *Transition,
	current State,
	command ModelSampleResultReceived,
) error {
	if err := requirePhase(current, command, PhaseSampling); err != nil {
		return err
	}
	if strings.TrimSpace(command.EffectID) == "" ||
		strings.TrimSpace(command.SampleID) == "" {
		return illegal(current, command, "sample result identity is incomplete")
	}
	if (command.Error == "") != (command.Failure == nil) {
		return illegal(current, command, "sample failure fact does not match result")
	}
	effect, ok := current.PendingEffects[command.EffectID]
	running := ok &&
		effect.Status == EffectRunning &&
		current.ActiveSampleID == command.SampleID
	scheduledAbort := ok &&
		effect.Status == EffectRequested &&
		command.Error != "" &&
		current.ActiveSampleID == "" &&
		current.SampleLedger[command.SampleID].Status == SampleRequested
	recoveredComplete := ok &&
		effect.Status == EffectRequested &&
		command.Error == "" &&
		current.ActiveSampleID == "" &&
		current.SampleLedger[command.SampleID].Status == SampleRequested &&
		current.SampleLedger[command.SampleID].Assembly != nil &&
		current.SampleLedger[command.SampleID].Assembly.State ==
			providerassembly.ResponseComplete
	if !ok ||
		effect.Kind != EffectSampleProvider ||
		effect.CallID != command.SampleID ||
		(!running && !scheduledAbort && !recoveredComplete) {
		return illegal(current, command, "sample result effect is not running")
	}
	success := command.Error == ""
	if err := finishEffect(
		transition,
		command.EffectID,
		success,
		command.Error,
	); err != nil {
		return illegal(current, command, err.Error())
	}
	sample := current.SampleLedger[command.SampleID]
	sample.Status = SampleCompleted
	sample.Error = ""
	sample.Retry = nil
	if !success {
		sample.Status = SampleFailed
		sample.Error = command.Error
		failure := *command.Failure
		sample.LastFailure = &failure
	}
	transition.State.SampleLedger[command.SampleID] = sample
	transition.State.ActiveSampleID = ""
	mergeUsage(&transition.State.Usage, command.Usage)
	transition.State.Context = command.Context
	transition.State.Context.Frozen = false
	transition.State.LastModelContinued = command.Continued
	transition.State.NextAction = StepActionNone
	transition.Events = append(transition.Events, Event{
		Kind: EventSampleFinished, SampleID: command.SampleID,
	})
	if !success {
		return nil
	}
	// Tool-prefacing narration is presentation, not a provisional final answer.
	// Completion declarations and convergence keep their existing output contract.
	commentary := strings.TrimSpace(command.Text) != "" &&
		len(command.Calls) != 0 && current.Convergence == nil &&
		!(len(command.Calls) == 1 && command.Calls[0].Name == "turn_complete")
	if commentary {
		callIDs := make([]string, 0, len(command.Calls))
		for _, call := range command.Calls {
			callIDs = append(callIDs, call.ID)
		}
		transition.State.Commentary = append(transition.State.Commentary, Commentary{
			SampleID: command.SampleID, Text: command.Text, CallIDs: callIDs,
		})
	} else if strings.TrimSpace(command.Text) != "" {
		transition.State.ProvisionalOutput = append(
			transition.State.ProvisionalOutput,
			command.Text,
		)
		transition.State.OutputEligibility = false
	}
	if len(command.Calls) != 0 {
		return applyToolCalls(
			transition,
			transition.State,
			ToolCallsProposed{Calls: command.Calls},
		)
	}
	transition.State.Progress.PendingIdentity = ""
	return nil
}

func applySupplementalUsage(
	transition *Transition,
	current State,
	command SupplementalUsageRecorded,
) error {
	if err := requirePhase(current, command, PhaseSampling); err != nil {
		return err
	}
	if strings.TrimSpace(command.Source) == "" ||
		strings.TrimSpace(command.SampleID) == "" {
		return illegal(
			current,
			command,
			"supplemental usage identity is incomplete",
		)
	}
	if command.Usage.Frozen {
		return illegal(current, command, "supplemental usage is frozen")
	}
	mergeUsage(&transition.State.Usage, command.Usage)
	transition.Events = append(transition.Events, Event{
		Kind: EventUsageRecorded, SampleID: command.SampleID,
	})
	return nil
}

func mergeUsage(target *UsageState, value UsageState) {
	calls := value.Calls
	if calls == 0 {
		calls = 1
	}
	if target.Calls == 0 {
		target.CostKnown = value.CostKnown
	} else {
		target.CostKnown = target.CostKnown && value.CostKnown
	}
	target.Calls += calls
	target.InputTokens += value.InputTokens
	target.OutputTokens += value.OutputTokens
	target.ReasoningTokens += value.ReasoningTokens
	target.CachedTokens += value.CachedTokens
	target.CostMicrounits += value.CostMicrounits
}

func applyProviderRetry(
	transition *Transition,
	current State,
	command ProviderRetryRequested,
) error {
	if err := requirePhase(current, command, PhaseSampling); err != nil {
		return err
	}
	effect, ok := current.PendingEffects[command.EffectID]
	if current.ActiveSampleID != command.SampleID ||
		!ok ||
		effect.Kind != EffectSampleProvider ||
		effect.CallID != command.SampleID ||
		effect.Status != EffectRunning ||
		command.Attempt == 0 ||
		command.Attempt != effect.Attempt ||
		command.Failure.Code == "" ||
		strings.TrimSpace(command.Failure.Message) == "" ||
		command.Retry == 0 ||
		strings.TrimSpace(command.PolicyRevision) == "" ||
		command.RetryAt.IsZero() {
		return illegal(current, command, "provider retry does not match active sample")
	}
	sample := current.SampleLedger[command.SampleID]
	if command.Retry != sample.ProviderRetries+1 {
		return illegal(current, command, "provider retry number is not monotonic")
	}
	failure := command.Failure
	sample.ProviderRetries = command.Retry
	sample.LastFailure = &failure
	sample.Retry = &ProviderRetryState{
		EffectID:         command.EffectID,
		Attempt:          command.Attempt,
		Retry:            command.Retry,
		EffectiveDelayMS: command.EffectiveDelayMS,
		RetryAt:          command.RetryAt,
		PolicyRevision:   command.PolicyRevision,
	}
	sample.Status = SampleRequested
	sample.Error = ""
	transition.State.SampleLedger[command.SampleID] = sample
	transition.State.ActiveSampleID = ""
	effect.Status = EffectRequested
	transition.State.PendingEffects[command.EffectID] = effect
	transition.Events = append(transition.Events, Event{
		Kind: EventProviderRetry, EffectID: command.EffectID,
		SampleID: command.SampleID,
	})
	return nil
}

func applyReleaseOutput(
	transition *Transition,
	current State,
) error {
	command := ReleaseProvisionalOutput{}
	if err := requirePhase(current, command, PhaseSampling); err != nil {
		return err
	}
	if len(current.ProvisionalOutput) == 0 {
		return illegal(current, command, "provisional output is empty")
	}
	if err := validateCompletionContract(current); err != nil {
		return illegal(current, command, err.Error())
	}
	transition.State.OutputEligibility = true
	transition.Events = append(
		transition.Events,
		Event{Kind: EventOutputReleased},
	)
	return nil
}

func applyDiscardOutput(
	transition *Transition,
	current State,
	command DiscardProvisionalOutput,
) error {
	if err := requirePhase(
		current,
		command,
		PhaseSampling,
		PhaseExecutingTools,
		PhaseAwaitingApproval,
		PhaseAwaitingInput,
		PhaseVerifying,
	); err != nil {
		return err
	}
	if strings.TrimSpace(command.Reason) == "" {
		return illegal(current, command, "discard reason is empty")
	}
	transition.State.ProvisionalOutput = nil
	transition.State.OutputEligibility = false
	transition.Events = append(
		transition.Events,
		Event{Kind: EventOutputDiscarded},
	)
	return nil
}

func applyRepairRequested(
	transition *Transition,
	current State,
	command RepairRequested,
) error {
	if err := requirePhase(current, command, PhaseSampling); err != nil {
		return err
	}
	if !validRepairKind(command.Kind) {
		return illegal(current, command, "repair kind is invalid")
	}
	if strings.TrimSpace(command.ProgressKey) == "" {
		return illegal(current, command, "repair progress key is empty")
	}
	if command.Limit == 0 {
		return illegal(current, command, "repair limit is zero")
	}
	return spendRepairBudget(
		transition,
		command.Kind,
		command.ProgressKey,
		command.Limit,
	)
}

func applyObserveProgress(
	transition *Transition,
	current State,
	command ObserveProgress,
) error {
	if err := requirePhase(current, command, PhaseSampling); err != nil {
		return err
	}
	if current.ActiveSampleID != "" {
		return illegal(current, command, "model sample is active")
	}
	signature := strings.TrimSpace(command.Signature)
	if signature == "" {
		return illegal(current, command, "progress signature is empty")
	}
	identity := strings.TrimSpace(command.SampleIdentity)
	if identity == "" {
		identity = current.Progress.PendingIdentity
	}
	progress := current.Progress
	if command.CompletedSamples < progress.ObservedSamples {
		return illegal(current, command, "completed samples regressed")
	}
	resultDigest := strings.TrimSpace(command.ResultDigest)
	if resultDigest == "" {
		resultDigest = FormatResultDigest(progress.PendingResultDigests)
	}
	observationKey := FormatObservationKey(
		signature,
		FormatWorkspaceContent(current.WorkItem),
		resultDigest,
	)
	revisited := seenObservation(progress, observationKey)
	if !revisited {
		progress.NoProgressSamples = 0
		progress.Stage = ProgressStageNone
		progress.StallKind = ProgressStallNone
		rememberObservation(&progress, observationKey)
	} else {
		progress.NoProgressSamples +=
			command.CompletedSamples - progress.ObservedSamples
		if liveToolIdentity(identity) && identity == progress.SampleIdentity {
			progress.StallKind = ProgressStallIdenticalCall
		} else {
			progress.StallKind = ProgressStallCycle
		}
	}
	progress.Signature = signature
	progress.SampleIdentity = identity
	progress.ObservationKey = observationKey
	progress.ObservedSamples = command.CompletedSamples
	policy := current.Policy.Convergence
	convergeAt := policy.ProgressConverge
	finishOnlyAt := policy.ProgressFinishOnly
	limit := policy.ProgressLimit
	if IsResearchIntent(current.Intent) &&
		current.MutationRevision == 0 {
		convergeAt = policy.ResearchConverge
		finishOnlyAt = policy.ResearchFinishOnly
		limit = policy.ResearchLimit
	}
	if progress.StallKind == ProgressStallIdenticalCall &&
		current.Policy.ImplementNoProgressSamples > 0 {
		lease := current.Policy.ImplementNoProgressSamples
		convergeAt = max(uint32(1), lease/2)
		finishOnlyAt = lease
		// The implement lease bounds an identical work-state repeat. A
		// seen observation with a different identity keeps the
		// MaxSteps-derived lease so A/B cycles cannot reset the clock.
		repairReserve := current.Policy.CompletionRepairLimit +
			current.Policy.WorkspaceRepairLimit +
			current.Policy.DeclarationRepairLimit +
			current.Policy.VerificationRepairLimit
		limit = max(lease+1, lease+repairReserve)
	}
	switch {
	case limit == 0:
		progress.Stage = ProgressStageNone
	case progress.NoProgressSamples >= limit:
		progress.Stage = ProgressStageExhausted
	case progress.NoProgressSamples >= finishOnlyAt:
		progress.Stage = ProgressStageFinishOnly
	case progress.NoProgressSamples >= convergeAt:
		progress.Stage = ProgressStageConverge
	default:
		progress.Stage = ProgressStageNone
	}
	transition.State.Progress = progress
	if progress.Stage == ProgressStageExhausted {
		used := progress.NoProgressSamples
		beginConvergence(
			transition,
			ConvergenceRequested{
				Cause: ConvergenceNoProgress,
				Used:  used,
				Limit: limit,
			},
		)
	}
	return nil
}

func applyConvergenceRequested(
	transition *Transition,
	current State,
	command ConvergenceRequested,
) error {
	if err := requirePhase(current, command, PhaseSampling); err != nil {
		return err
	}
	if current.ActiveSampleID != "" {
		return illegal(current, command, "model sample is active")
	}
	if !validConvergenceRequest(command) {
		return illegal(current, command, "convergence request is invalid")
	}
	beginConvergence(transition, command)
	return nil
}

func applyConvergenceFinalizationStarted(
	transition *Transition,
	current State,
) error {
	command := ConvergenceFinalizationStarted{}
	if err := requirePhase(current, command, PhaseSampling); err != nil {
		return err
	}
	if current.ActiveSampleID != "" {
		return illegal(current, command, "model sample is active")
	}
	if current.Convergence == nil {
		return illegal(current, command, "convergence is not active")
	}
	if current.Convergence.FinalizationAttempted {
		return illegal(current, command, "convergence finalization already started")
	}
	transition.State.Convergence.FinalizationAttempted = true
	transition.State.NextAction = StepActionNone
	return nil
}

func beginConvergence(
	transition *Transition,
	command ConvergenceRequested,
) {
	if transition.State.Convergence != nil {
		return
	}
	summary := ""
	pending := []string(nil)
	if completion := transition.State.Completion; completion != nil {
		summary = strings.TrimSpace(completion.Summary)
		pending = append([]string(nil), completion.PendingActions...)
	}
	if summary != "" && len(pending) == 0 {
		pending = []string{
			"Continue the unfinished work from the retained turn context.",
		}
	}
	transition.State.Convergence = &ConvergenceState{
		Cause:          command.Cause,
		Used:           command.Used,
		Limit:          command.Limit,
		RepairKind:     command.RepairKind,
		Summary:        summary,
		PendingActions: pending,
	}
	transition.State.NextAction = StepActionFinalize
	transition.Events = append(
		transition.Events,
		Event{Kind: EventConvergence},
	)
}

func validConvergenceRequest(command ConvergenceRequested) bool {
	if command.Used < command.Limit || command.Limit == 0 &&
		(command.Used != 0 || command.Cause != ConvergenceRepairBudget) {
		return false
	}
	switch command.Cause {
	case ConvergenceOutputLimit,
		ConvergenceNoProgress,
		ConvergenceStepLimit:
		return command.RepairKind == ""
	case ConvergenceRepairBudget:
		return validRepairKind(command.RepairKind)
	default:
		return false
	}
}

func spendRepairBudget(
	transition *Transition,
	kind RepairKind,
	progressKey string,
	limit uint32,
) error {
	if !validRepairKind(kind) {
		return fmt.Errorf("invalid repair kind %q", kind)
	}
	if strings.TrimSpace(progressKey) == "" || limit == 0 {
		return errors.New("repair budget request is incomplete")
	}
	budget := transition.State.RepairBudgets[kind]
	if budget.ProgressKey != progressKey {
		budget.ProgressKey = progressKey
		budget.Consecutive = 0
	}
	if budget.Consecutive >= limit {
		return &RepairBudgetExhaustedError{
			Kind: kind, ProgressKey: progressKey, Limit: limit,
		}
	}
	budget.Consecutive++
	budget.Steps++
	transition.State.RepairBudgets[kind] = budget
	transition.Events = append(
		transition.Events,
		Event{Kind: EventRepairRequested},
	)
	return nil
}

func validRepairKind(kind RepairKind) bool {
	switch kind {
	case RepairCompletion, RepairWorkspace, RepairDeclaration, RepairVerification:
		return true
	default:
		return false
	}
}

func blockedConvergence(source *ConvergenceState) *ConvergenceState {
	outcome := cloneConvergence(source)
	if outcome == nil {
		return nil
	}
	if strings.TrimSpace(outcome.Summary) == "" {
		outcome.Summary = fmt.Sprintf(
			"Turn could not complete after the %s convergence budget was exhausted.",
			outcome.Cause,
		)
		outcome.PendingActions = []string{
			"Continue the unfinished work from the retained turn context.",
		}
	}
	return outcome
}
