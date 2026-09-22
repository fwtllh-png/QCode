package turnkernel

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"slices"
	"strings"
	"sync"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// RuntimeKernel adapts the existing Engine loop to the authoritative
// TurnCoordinator. The state field is a read-only snapshot refreshed only
// after Coordinator accepts a durable transition.
type RuntimeKernel struct {
	mu          sync.Mutex
	state       State
	coordinator *TurnCoordinator
	dispatcher  *DurableEffectDispatcher
	observe     func(TransitionRecord)
	sink        func(TransitionRecord)
	metrics     RuntimeKernelMetrics
	restored    bool
}

type KernelIdentity struct {
	TurnID          string
	ProfileRevision uint64
	Goal            string
	WorkItem        WorkItem
}

type RuntimeKernelMetrics interface {
	TurnKernelObserver(bool, bool)
}

type ProgressObservation struct {
	Stage             ProgressStage
	ObservedSamples   uint32
	NoProgressSamples uint32
	ReadOnlyResearch  bool
	StageChanged      bool
	StallKind         ProgressStallKind
}

type FrozenTerminalState struct {
	TurnID      string
	State       State
	DomainFacts []DomainFact
}

func NewRuntimeKernel(
	identity KernelIdentity,
	intent protocol.TurnIntent,
	mode string,
	recovery *protocol.TurnRecoveryContext,
	draftResumed bool,
	draftChanges []ObservedChange,
	observe func(TransitionRecord),
	sink func(TransitionRecord),
	factObserver DomainFactObserver,
	metrics RuntimeKernelMetrics,
	policy Policy,
	runtime CoordinatorRuntime,
) (*RuntimeKernel, error) {
	turnID := identity.TurnID
	profileRevision := identity.ProfileRevision
	state := NewStateWithPolicy(
		intent,
		mode,
		profileRevision,
		policy,
	)
	if runtime == nil {
		return nil, fmt.Errorf("turn coordinator runtime is nil")
	}
	handle, err := runtime.Open(
		context.Background(),
		turnID,
		state,
	)
	if err != nil {
		return nil, err
	}
	coordinator := handle.Coordinator
	coordinator.SetDomainFactObserver(factObserver)
	dispatcher := handle.Dispatcher
	kernel := &RuntimeKernel{
		state:       coordinator.Snapshot(),
		coordinator: coordinator,
		dispatcher:  dispatcher,
		observe:     observe,
		sink:        sink,
		metrics:     metrics,
		restored:    handle.Restored,
	}
	if handle.Restored {
		return kernel, nil
	}
	if recovery != nil {
		if err := kernel.applyAuthoritative(RecoveryRequested{
			SourceTurnID:           string(recovery.SourceTurnID),
			RecoveryTurnID:         turnID,
			CurrentProfileRevision: profileRevision,
			Action:                 string(recovery.Action),
			DraftResumed:           draftResumed,
			Changes:                append([]ObservedChange(nil), draftChanges...),
		}); err != nil {
			return nil, err
		}
	}
	if identity.Goal != "" || identity.WorkItem.HasKnownOrOpen() {
		if err := kernel.applyAuthoritative(BindWorkItem{
			Goal:       identity.Goal,
			KnownReads: identity.WorkItem.KnownReads,
			KnownEdits: identity.WorkItem.KnownEdits,
			Open:       identity.WorkItem.Open,
		}); err != nil {
			return nil, err
		}
	}
	if err := kernel.applyAuthoritative(StartTurn{}); err != nil {
		return nil, err
	}
	if err := kernel.applyAuthoritative(
		PreparationFinished{},
	); err != nil {
		return nil, err
	}
	return kernel, nil
}
func (s *RuntimeKernel) PendingSampleID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, effect := range s.dispatcher.PendingRouted(
		EffectSampleProvider,
	) {
		return effect.CallID
	}
	return ""
}

func (s *RuntimeKernel) Phase() Phase {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.Phase
}

func (s *RuntimeKernel) Snapshot() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.coordinator.Snapshot()
}

func (s *RuntimeKernel) RoutedEffect(
	kind EffectKind,
	callID string,
) (Effect, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dispatcher.Routed(kind, callID)
}

func (s *RuntimeKernel) PendingRouted(kind EffectKind) []Effect {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dispatcher.PendingRouted(kind)
}

func (s *RuntimeKernel) FrozenTerminalState(
	ctx context.Context,
) (FrozenTerminalState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	facts, err := s.coordinator.DomainFacts(ctx)
	if err != nil {
		return FrozenTerminalState{}, err
	}
	state := s.coordinator.Snapshot()
	if !state.Phase.Terminal() {
		return FrozenTerminalState{}, errors.New("turn kernel is not terminal")
	}
	return FrozenTerminalState{
		TurnID: s.coordinator.TurnID(), State: state, DomainFacts: facts,
	}, nil
}
func (s *RuntimeKernel) HasSample(sampleID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, exists := s.state.SampleLedger[sampleID]
	return exists
}
func (s *RuntimeKernel) CommittingDecision() (
	TerminalDecision,
	bool,
) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Phase != PhaseCommitting ||
		s.state.PendingTerminal == nil {
		return TerminalDecision{}, false
	}
	return *s.state.PendingTerminal, true
}
func (s *RuntimeKernel) FrozenOutput() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.state.FinalOutput, "")
}
func (s *RuntimeKernel) TerminalDecision() (
	TerminalDecision,
	bool,
) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.state.Phase.Terminal() || s.state.Terminal == nil {
		return TerminalDecision{}, false
	}
	return *s.state.Terminal, true
}
func (s *RuntimeKernel) PendingToolCalls() []provider.ToolCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	effects := s.dispatcher.PendingRouted(EffectExecuteTool)
	calls := make([]provider.ToolCall, 0, len(effects))
	for _, effect := range effects {
		call, ok := s.state.OpenCalls[effect.CallID]
		if !ok {
			continue
		}
		calls = append(calls, provider.ToolCall{
			ID: call.ID, Name: call.Name, Arguments: call.Arguments,
			CatalogID:         call.CatalogID,
			CatalogGeneration: call.CatalogGeneration,
			CatalogRevision:   call.CatalogRevision,
			CatalogAuthority:  call.CatalogAuthority,
		})
	}
	return calls
}
func (s *RuntimeKernel) StartTools(calls []provider.ToolCall) error {
	if s == nil || len(calls) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	states := make([]ToolCallState, 0, len(calls))
	for _, call := range calls {
		if existing, ok := s.state.OpenCalls[call.ID]; ok {
			if existing.Name != call.Name {
				return fmt.Errorf(
					"tool call %q name changed from %q to %q",
					call.ID,
					existing.Name,
					call.Name,
				)
			}
			continue
		}
		states = append(states, ToolCallState{
			ID: call.ID, Name: call.Name,
			Arguments:         call.Arguments,
			CatalogID:         call.CatalogID,
			CatalogGeneration: call.CatalogGeneration,
			CatalogRevision:   call.CatalogRevision,
			CatalogAuthority:  call.CatalogAuthority,
		})
	}
	if len(states) == 0 {
		return nil
	}
	return s.applyAuthoritativeLocked(ToolCallsProposed{Calls: states})
}
func (s *RuntimeKernel) StartTool(callID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	from := s.state.Phase
	effect, err := s.dispatcher.Start(
		EffectExecuteTool,
		callID,
	)
	if err != nil {
		return err
	}
	s.recordAcceptedLocked(EffectStarted{
		EffectID: effect.ID,
		Attempt:  effect.Attempt,
	}, from)
	return nil
}
func (s *RuntimeKernel) ValidateToolStarts(
	calls []provider.ToolCall,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := make(map[string]struct{}, len(calls))
	for _, call := range calls {
		if call.ID == "" || call.Name == "" {
			return protocol.NewProblem(
				protocol.CodeConflict,
				"tool call identity is incomplete",
				false,
				nil,
			)
		}
		if _, duplicate := seen[call.ID]; duplicate {
			return protocol.NewProblem(
				protocol.CodeConflict,
				fmt.Sprintf("duplicate tool call id %q", call.ID),
				false,
				nil,
			)
		}
		seen[call.ID] = struct{}{}
		if open, exists := s.state.OpenCalls[call.ID]; exists {
			status := EffectLifecycleStatus("")
			for _, effect := range s.state.PendingEffects {
				if effect.Kind == EffectExecuteTool &&
					effect.CallID == call.ID {
					status = effect.Status
					break
				}
			}
			if open.Name != call.Name ||
				status != EffectRequested {
				return protocol.NewProblem(
					protocol.CodeConflict,
					fmt.Sprintf("tool call id %q is already open", call.ID),
					false,
					nil,
				)
			}
		}
		if _, closed := s.state.ClosedCalls[call.ID]; closed {
			return protocol.NewProblem(
				protocol.CodeConflict,
				fmt.Sprintf("tool call id %q is already closed", call.ID),
				false,
				nil,
			)
		}
	}
	return nil
}
func (s *RuntimeKernel) CloseTool(
	call provider.ToolCall,
	result tool.Result,
	fileChanges []tool.WorkspaceChange,
) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	effect := routedEffectByCall(
		s.dispatcher.PendingRouted(EffectExecuteTool),
		call.ID,
	)
	if effect.ID == "" {
		return fmt.Errorf("tool effect for call %q is not routed", call.ID)
	}
	if len(fileChanges) == 0 {
		fileChanges = ObservedFileChanges(result)
	}
	changes := ObservedChanges(fileChanges)
	open, _ := s.state.OpenCalls[call.ID]
	command := ToolResultReceived{
		EffectID:    effect.ID,
		CallID:      call.ID,
		IsError:     result.IsError,
		Changes:     changes,
		Observation: ObserveWorkItemResult(open, result),
	}
	from := s.state.Phase
	if err := s.dispatcher.Resolve(command); err != nil {
		return err
	}
	s.recordAcceptedLocked(command, from)
	return nil
}
func (s *RuntimeKernel) AbortTools(reason string) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.state.OpenCalls) == 0 {
		return nil
	}
	requestIDs := make([]string, 0, len(s.state.PendingApprovals))
	for requestID := range s.state.PendingApprovals {
		requestIDs = append(requestIDs, requestID)
	}
	slices.Sort(requestIDs)
	for _, requestID := range requestIDs {
		approval := s.state.PendingApprovals[requestID]
		effect, started, err := s.dispatcher.Routed(
			EffectAwaitApproval,
			approval.CallID,
		)
		if err != nil {
			return fmt.Errorf("abort approval effect: %w", err)
		}
		if !started {
			effect, err = s.dispatcher.Start(
				EffectAwaitApproval,
				approval.CallID,
			)
			if err != nil {
				return fmt.Errorf("start approval effect for abort: %w", err)
			}
		}
		if err := s.dispatcher.Resolve(ApprovalResultReceived{
			EffectID: effect.ID, RequestID: requestID,
			Accepted: false, Error: reason,
		}); err != nil {
			return fmt.Errorf("abort approval effect: %w", err)
		}
	}
	s.state = s.coordinator.Snapshot()
	if err := s.dispatcher.Abort(
		EffectExecuteTool,
		"",
		reason,
	); err != nil {
		return fmt.Errorf("abort execute_tool effect: %w", err)
	}
	s.state = s.coordinator.Snapshot()
	if len(s.state.OpenCalls) == 0 {
		return nil
	}
	return s.applyAuthoritativeLocked(AbortOpenCalls{Reason: reason})
}
func (s *RuntimeKernel) MutationRevision() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.MutationRevision
}
func (s *RuntimeKernel) VerificationMustPass() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.Policy.VerificationMustPass
}
func (s *RuntimeKernel) Completion() *CompletionDecision {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Completion == nil {
		return nil
	}
	copy := *s.state.Completion
	copy.PendingActions = append([]string(nil), copy.PendingActions...)
	copy.ChangedPaths = append([]string(nil), copy.ChangedPaths...)
	copy.VerificationCalls = append([]string(nil), copy.VerificationCalls...)
	return &copy
}
func (s *RuntimeKernel) Convergence() *ConvergenceState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Convergence == nil {
		return nil
	}
	copy := *s.state.Convergence
	copy.PendingActions = append(
		[]string(nil),
		s.state.Convergence.PendingActions...,
	)
	return &copy
}
func (s *RuntimeKernel) HasProvisionalOutput() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.state.ProvisionalOutput) != 0
}
func (s *RuntimeKernel) RequestConvergence(
	request ConvergenceRequested,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Convergence != nil {
		return nil
	}
	return s.applyAuthoritativeLocked(request)
}
func (s *RuntimeKernel) BeginConvergenceFinalization() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applyAuthoritativeLocked(
		ConvergenceFinalizationStarted{},
	)
}
func (s *RuntimeKernel) CompletionDeclaration() *tool.CompletionDeclaration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Completion == nil || !s.state.Completion.Accepted {
		return nil
	}
	decision := s.state.Completion
	return &tool.CompletionDeclaration{
		Status:              "complete",
		Summary:             decision.Summary,
		OutputMode:          decision.OutputMode,
		ChangedPaths:        append([]string(nil), decision.ChangedPaths...),
		VerificationCallIDs: append([]string(nil), decision.VerificationCalls...),
		MutationRevision:    decision.Mutation,
		CallID:              decision.CompletionCall,
	}
}
func (s *RuntimeKernel) BlockedCompletionDeclaration() *tool.CompletionDeclaration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Convergence == nil ||
		strings.TrimSpace(s.state.Convergence.Summary) == "" ||
		len(s.state.Convergence.PendingActions) == 0 {
		return nil
	}
	return &tool.CompletionDeclaration{
		Status:  "incomplete",
		Summary: s.state.Convergence.Summary,
		PendingActions: append(
			[]string(nil),
			s.state.Convergence.PendingActions...,
		),
	}
}
func (s *RuntimeKernel) EvaluateCompletion(
	candidate CompletionCandidate,
) (CompletionDecision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.applyAuthoritativeLocked(CompletionEvaluated{
		Candidate: candidate,
	}); err != nil {
		return CompletionDecision{}, err
	}
	if s.state.Completion == nil {
		return CompletionDecision{}, errors.New(
			"reducer did not produce a completion decision",
		)
	}
	decision := *s.state.Completion
	decision.ChangedPaths = append([]string(nil), decision.ChangedPaths...)
	decision.VerificationCalls = append([]string(nil), decision.VerificationCalls...)
	return decision, nil
}

func (s *RuntimeKernel) InvalidateCompletion(reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applyAuthoritativeLocked(CompletionInvalidated{
		Reason: reason,
	})
}

func (s *RuntimeKernel) RequireApproval(
	requestID string,
	callID string,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.applyAuthoritativeLocked(ApprovalRequired{
		RequestID: requestID,
		CallID:    callID,
	}); err != nil {
		return err
	}
	from := s.state.Phase
	effect, err := s.dispatcher.Start(
		EffectAwaitApproval,
		callID,
	)
	if err != nil {
		return err
	}
	s.recordAcceptedLocked(EffectStarted{
		EffectID: effect.ID,
		Attempt:  effect.Attempt,
	}, from)
	return nil
}

func (s *RuntimeKernel) EnsureApproval(
	requestID string,
	callID string,
) error {
	s.mu.Lock()
	current, ok := s.state.PendingApprovals[requestID]
	s.mu.Unlock()
	if ok {
		if current.CallID != callID {
			return fmt.Errorf(
				"approval %s belongs to call %s, not %s",
				requestID, current.CallID, callID,
			)
		}
		return nil
	}
	return s.RequireApproval(requestID, callID)
}

func (s *RuntimeKernel) ResolveApproval(
	requestID string,
	canceled bool,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	approval, ok := s.state.PendingApprovals[requestID]
	if !ok {
		return fmt.Errorf("approval request %q is not pending", requestID)
	}
	effect, started, err := s.dispatcher.Routed(
		EffectAwaitApproval,
		approval.CallID,
	)
	if err != nil {
		return err
	}
	if !started {
		from := s.state.Phase
		effect, err = s.dispatcher.Start(
			EffectAwaitApproval,
			approval.CallID,
		)
		if err != nil {
			return err
		}
		s.recordAcceptedLocked(EffectStarted{
			EffectID: effect.ID,
			Attempt:  effect.Attempt,
		}, from)
	}
	command := ApprovalResultReceived{
		EffectID:  effect.ID,
		RequestID: requestID,
		Accepted:  true,
		Canceled:  canceled,
	}
	from := s.state.Phase
	if err := s.dispatcher.Resolve(command); err != nil {
		return err
	}
	s.recordAcceptedLocked(command, from)
	return nil
}

func (s *RuntimeKernel) RequireInput(requestID, callID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.applyAuthoritativeLocked(InputRequired{
		RequestID: requestID,
		CallID:    callID,
	}); err != nil {
		return err
	}
	from := s.state.Phase
	effect, err := s.dispatcher.Start(EffectAwaitInput, "")
	if err != nil {
		return err
	}
	s.recordAcceptedLocked(EffectStarted{
		EffectID: effect.ID,
		Attempt:  effect.Attempt,
	}, from)
	return nil
}

func (s *RuntimeKernel) EnsureInput(requestID, callID string) error {
	s.mu.Lock()
	pending := s.state.PendingInput
	s.mu.Unlock()
	if pending != nil {
		if pending.RequestID != requestID {
			return fmt.Errorf(
				"input wait belongs to request %s, not %s",
				pending.RequestID, requestID,
			)
		}
		return nil
	}
	return s.RequireInput(requestID, callID)
}

func (s *RuntimeKernel) ResolveInput(requestID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending := s.state.PendingInput
	if pending == nil || pending.RequestID != requestID {
		return fmt.Errorf("input request %q is not pending", requestID)
	}
	effect, started, err := s.dispatcher.Routed(
		EffectAwaitInput,
		"",
	)
	if err != nil {
		return err
	}
	if !started {
		from := s.state.Phase
		effect, err = s.dispatcher.Start(EffectAwaitInput, "")
		if err != nil {
			return err
		}
		s.recordAcceptedLocked(EffectStarted{
			EffectID: effect.ID,
			Attempt:  effect.Attempt,
		}, from)
	}
	command := InputResultReceived{
		EffectID:  effect.ID,
		RequestID: requestID,
		Accepted:  true,
	}
	from := s.state.Phase
	if err := s.dispatcher.Resolve(command); err != nil {
		return err
	}
	s.recordAcceptedLocked(command, from)
	return nil
}

func (s *RuntimeKernel) RequestCancel(reason string) error {
	return s.applyAuthoritative(CancelRequested{Reason: reason})
}

// RecordContinuation commits the cursor of a stored conversation snapshot.
// The caller must have written the referenced content before committing the
// cursor, so a committed fact never points at unreadable content.
func (s *RuntimeKernel) RecordContinuation(cursor ContinuationCursor) error {
	return s.applyAuthoritative(ContinuationRecorded{Cursor: cursor})
}

func (s *RuntimeKernel) ContinuationCursor() (ContinuationCursor, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Continuation == nil {
		return ContinuationCursor{}, false
	}
	return *s.state.Continuation, true
}

// NextContinuationSequence returns the sequence a new continuation snapshot
// must carry to extend the durable cursor.
func (s *RuntimeKernel) NextContinuationSequence() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Continuation == nil {
		return 1
	}
	return s.state.Continuation.Sequence + 1
}

func (s *RuntimeKernel) CancellationReason() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.state.Cancellation.Accepted {
		return ""
	}
	return s.state.Cancellation.Reason
}

func (s *RuntimeKernel) BeginVerification() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applyAuthoritativeLocked(VerificationStarted{})
}

func (s *RuntimeKernel) FinishVerification(
	command VerificationFinished,
) (VerificationAction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	from := s.state.Phase
	effect, err := s.dispatcher.Start(
		EffectRunVerification,
		"",
	)
	if err != nil {
		return "", err
	}
	s.recordAcceptedLocked(EffectStarted{
		EffectID: effect.ID,
		Attempt:  effect.Attempt,
	}, from)
	command.EffectID = effect.ID
	from = s.state.Phase
	if err := s.dispatcher.Resolve(command); err != nil {
		return "", err
	}
	s.recordAcceptedLocked(command, from)
	return s.state.Verification.Action, nil
}

func (s *RuntimeKernel) BufferOutput(text string) error {
	if text == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applyAuthoritativeLocked(ModelTextReceived{Text: text})
}

func (s *RuntimeKernel) ReleaseOutput() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	output := append([]string(nil), s.state.ProvisionalOutput...)
	if err := s.applyAuthoritativeLocked(
		ReleaseProvisionalOutput{},
	); err != nil {
		return nil, err
	}
	return output, nil
}

func (s *RuntimeKernel) DiscardOutput(reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.state.ProvisionalOutput) == 0 {
		return nil
	}
	return s.applyAuthoritativeLocked(DiscardProvisionalOutput{
		Reason: reason,
	})
}

func (s *RuntimeKernel) RepairSteps(
	kind RepairKind,
) uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.RepairBudgets[kind].Steps
}

func (s *RuntimeKernel) ValidateFinalReadiness() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if RequiresCompletion(s.state) &&
		(s.state.Completion == nil || !s.state.Completion.Accepted) {
		return protocol.NewProblem(
			protocol.CodeConflict,
			"kernel final readiness requires accepted completion",
			false,
			nil,
		)
	}
	if s.state.Policy.VerificationRequired &&
		s.state.MutationRevision != 0 &&
		(s.state.Verification.Mutation != s.state.MutationRevision ||
			(s.state.Verification.Action != VerificationActionPassed &&
				s.state.Verification.Action != VerificationActionReported &&
				s.state.Verification.Action != VerificationActionReverted)) {
		return protocol.NewProblem(
			protocol.CodeConflict,
			"kernel final readiness requires passed verification",
			false,
			nil,
		)
	}
	return nil
}

func (s *RuntimeKernel) RepairProgressKey() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return FormatRepairProgressKey(s.state)
}

func (s *RuntimeKernel) Intent() protocol.TurnIntent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.Intent
}

func (s *RuntimeKernel) ProgressSignature(
	completedPlanSteps int,
	openImplement bool,
) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return FormatProgressSignature(s.state, completedPlanSteps, openImplement)
}

func (s *RuntimeKernel) WorkItem() WorkItem {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneWorkItem(s.state.WorkItem)
}

func (s *RuntimeKernel) BindWorkItem(bind BindWorkItem) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applyAuthoritativeLocked(bind)
}

func (s *RuntimeKernel) RecoveryContinue() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.RecoveryRelation != nil &&
		s.state.RecoveryRelation.Action == string(protocol.TurnRecoveryContinue)
}

// FormatRepairProgressKey names the current declaration-repair world. Rejected
// completion call identities and extra closed tools are not progress.
func FormatRepairProgressKey(state State) string {
	accepted := false
	reason := ""
	if state.Completion != nil {
		accepted = state.Completion.Accepted
		if !accepted {
			reason = state.Completion.Reason
		}
	}
	operationCalls, lifecycleCalls := progressToolCounts(state)
	return fmt.Sprintf(
		"mutation=%d;completion_accepted=%t;completion_reason=%s;"+
			"verification=%s;operation_calls=%d;lifecycle_calls=%d",
		state.MutationRevision,
		accepted,
		reason,
		state.Verification.Status,
		operationCalls,
		lifecycleCalls,
	)
}

// FormatProgressSignature names the public Work Item path-set. Same-path
// edits, rejected declarations, and call counts do not change it. Anti-spin
// still fingerprints workspace content versions and result digests.
func FormatProgressSignature(
	state State,
	completedPlanSteps int,
	openImplement bool,
) string {
	return FormatWorkItemSignature(state, completedPlanSteps, openImplement)
}

func progressToolCounts(state State) (operationCalls int, lifecycleCalls int) {
	for _, result := range state.ClosedCalls {
		if result.IsError {
			continue
		}
		if state.Intent == protocol.TurnIntentOperation {
			operationCalls++
		}
		if agentLifecycleProgressTool(result.Name) {
			lifecycleCalls++
		}
	}
	return operationCalls, lifecycleCalls
}

func agentLifecycleProgressTool(name string) bool {
	switch name {
	case "spawn_agent",
		"send_message",
		"wait_agent",
		"followup_task",
		"interrupt_agent",
		"close_agent",
		"integrate_agent",
		"request_user_input":
		return true
	default:
		return false
	}
}

func (s *RuntimeKernel) ObserveProgress(
	signature string,
	sampleIdentity string,
) (ProgressObservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	completed := uint32(0)
	for _, sample := range s.state.SampleLedger {
		if sample.Status == SampleCompleted {
			completed++
		}
	}
	current := s.state.Progress
	if completed < current.ObservedSamples {
		return ProgressObservation{}, fmt.Errorf(
			"completed model samples regressed from %d to %d",
			current.ObservedSamples,
			completed,
		)
	}
	previousStage := current.Stage
	if err := s.applyAuthoritativeLocked(ObserveProgress{
		Signature:        signature,
		SampleIdentity:   sampleIdentity,
		CompletedSamples: completed,
	}); err != nil {
		return ProgressObservation{}, err
	}
	return ProgressObservation{
		Stage:             s.state.Progress.Stage,
		ObservedSamples:   s.state.Progress.ObservedSamples,
		NoProgressSamples: s.state.Progress.NoProgressSamples,
		ReadOnlyResearch: IsResearchIntent(s.state.Intent) &&
			s.state.MutationRevision == 0,
		StageChanged: s.state.Progress.Stage != previousStage,
		StallKind:    s.state.Progress.StallKind,
	}, nil
}

func (s *RuntimeKernel) ProgressObservation() ProgressObservation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ProgressObservation{
		Stage:             s.state.Progress.Stage,
		ObservedSamples:   s.state.Progress.ObservedSamples,
		NoProgressSamples: s.state.Progress.NoProgressSamples,
		ReadOnlyResearch: IsResearchIntent(s.state.Intent) &&
			s.state.MutationRevision == 0,
		StallKind: s.state.Progress.StallKind,
	}
}

func ObservedChanges(
	fileChanges []tool.WorkspaceChange,
) []ObservedChange {
	changes := make([]ObservedChange, 0, len(fileChanges))
	for _, change := range fileChanges {
		changes = append(changes, ObservedChange{
			Path:          change.Path,
			Kind:          string(change.Kind),
			ContentDigest: strings.TrimSpace(change.AfterDigest),
		})
	}
	return changes
}

func (s *RuntimeKernel) recordAcceptedLocked(
	command Command,
	from Phase,
) {
	s.state = s.coordinator.Snapshot()
	record := TransitionRecord{
		Command: CommandName(command),
		From:    from,
		To:      s.state.Phase,
	}
	record.StateDigest, record.Drift = s.committedDigestLocked()
	s.recordLocked(record)
}

// committedDigestLocked prefers the coordinator's cached digest of the last
// committed transition; recomputing it here would reserialize the full state
// on every command. Drift is set only when no committed digest exists yet and
// the local snapshot cannot be digested.
func (s *RuntimeKernel) committedDigestLocked() (digest string, drift string) {
	if cached, ok := s.coordinator.LastDigest(); ok {
		return cached, ""
	}
	computed, err := Digest(s.state)
	if err != nil {
		return "", err.Error()
	}
	return computed, ""
}

func (s *RuntimeKernel) applyAuthoritative(
	command Command,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applyAuthoritativeLocked(command)
}

func (s *RuntimeKernel) applyAuthoritativeLocked(
	command Command,
) error {
	from := s.state.Phase
	err := s.coordinator.Submit(context.Background(), command)
	record := TransitionRecord{
		Command: CommandName(command),
		From:    from,
		To:      from,
	}
	if err != nil {
		record.Rejection = err.Error()
		record.StateDigest, _ = s.committedDigestLocked()
		s.recordLocked(record)
		var problem *protocol.Problem
		if errors.As(err, &problem) &&
			problem.Code != protocol.CodeInternal {
			return problem
		}
		return protocol.NewFault(
			protocol.CodeConflict,
			"turn kernel rejected an authoritative command",
			true,
			protocol.FaultMetadata{
				Origin:         protocol.FaultOriginKernel,
				Disposition:    protocol.FaultRetryStep,
				SideEffects:    protocol.SideEffectUnchanged,
				RecoveryAction: "reconcile the command with the durable turn state",
			},
			err,
		)
	}
	s.state = s.coordinator.Snapshot()
	record.To = s.state.Phase
	record.StateDigest, record.Drift = s.committedDigestLocked()
	if record.Drift != "" {
		s.recordLocked(record)
		return protocol.NewFault(
			protocol.CodeInternal,
			"turn kernel could not digest authoritative command",
			false,
			protocol.FaultMetadata{
				Origin:      protocol.FaultOriginKernel,
				Disposition: protocol.FaultFailTurn,
				SideEffects: protocol.SideEffectUnknown,
			},
			errors.New(record.Drift),
		)
	}
	s.recordLocked(record)
	return nil
}

func (s *RuntimeKernel) recordLocked(record TransitionRecord) {
	if s.metrics != nil {
		s.metrics.TurnKernelObserver(
			record.Drift != "",
			record.StateDigest == "" && record.Drift != "",
		)
	}
	if s.observe != nil {
		s.observe(record)
	}
	if s.sink == nil {
		return
	}
	func() {
		defer func() {
			if recover() != nil {
				_ = debug.Stack()
			}
		}()
		s.sink(record)
	}()
}
