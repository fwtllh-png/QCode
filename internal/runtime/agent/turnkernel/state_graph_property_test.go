package turnkernel

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestStateGraphCanonicalPathsCoverEveryPhaseAndTerminal(t *testing.T) {
	visited := make(map[Phase]bool)
	record := func(state State) State {
		t.Helper()
		if err := Validate(state); err != nil {
			t.Fatalf("invalid %s state: %v", state.Phase, err)
		}
		visited[state.Phase] = true
		return state
	}

	completed := record(NewState(protocol.TurnIntentAnswer, "act", 1))
	completed = record(apply(t, completed, StartTurn{}).State)
	completed = record(apply(t, completed, PreparationFinished{}).State)
	completed = record(apply(t, completed, ModelTextReceived{Text: "answer"}).State)
	completed = record(apply(t, completed, ReleaseProvisionalOutput{}).State)
	completed = record(apply(t, completed, TerminalRequested{}).State)
	completed = record(apply(t, completed, FinishTerminal{}).State)

	canceled := record(startSampling(t, protocol.TurnIntentAnswer))
	canceled = record(apply(t, canceled, ToolCallsProposed{
		Calls: []ToolCallState{{ID: "call-1", Name: "shell_read"}},
	}).State)
	canceled = record(apply(t, canceled, ApprovalRequired{
		RequestID: "approval-1", CallID: "call-1",
	}).State)
	canceled = record(apply(t, canceled, ApprovalResolved{
		RequestID: "approval-1",
	}).State)
	canceled = record(apply(t, canceled, ToolResultReceived{
		CallID: "call-1",
	}).State)
	canceled = record(apply(t, canceled, TerminalRequested{
		CancelReason: "user canceled",
	}).State)
	canceled = record(apply(t, canceled, FinishTerminal{}).State)

	failed := record(startSampling(t, protocol.TurnIntentAnswer))
	failed = record(apply(t, failed, InputRequired{
		RequestID: "input-1",
	}).State)
	failed = record(apply(t, failed, InputResolved{
		RequestID: "input-1",
	}).State)
	failed = record(apply(t, failed, TerminalRequested{
		FailureMessage: "fixture failure",
	}).State)
	failed = record(apply(t, failed, FinishTerminal{}).State)

	verified := record(startSampling(t, protocol.TurnIntentWorkspaceChange))
	verified = record(apply(t, verified, ToolCallsProposed{
		Calls: []ToolCallState{{ID: "write-1", Name: "file_write"}},
	}).State)
	verified = record(apply(t, verified, ToolResultReceived{
		CallID: "write-1",
		Changes: []ObservedChange{{
			Path: "state.go", Kind: "modified",
		}},
	}).State)
	verified = record(apply(t, verified, CompletionEvaluated{
		Candidate: CompletionCandidate{
			DeclarationValid: true,
			Status:           "complete",
			Summary:          "implemented",
			CompletionCall:   "complete-1",
			BatchSize:        1,
		},
	}).State)

	wantPhases := []Phase{
		PhaseCreated,
		PhasePreparing,
		PhaseSampling,
		PhaseExecutingTools,
		PhaseAwaitingApproval,
		PhaseAwaitingInput,
		PhaseCommitting,
		PhaseCompleted,
		PhaseFailed,
		PhaseCanceled,
	}
	for _, phase := range wantPhases {
		if !visited[phase] {
			t.Errorf("canonical state graph did not reach phase %q", phase)
		}
	}
	for _, terminal := range []State{completed, failed, canceled} {
		assertTerminalRejectsEveryLateCommand(t, terminal)
	}
}

func TestStateGraphDuplicateLateAndReorderedCommandMatrix(t *testing.T) {
	sampling := startSampling(t, protocol.TurnIntentAnswer)
	tools := apply(t, sampling, ToolCallsProposed{
		Calls: []ToolCallState{
			{ID: "call-a", Name: "first"},
			{ID: "call-b", Name: "second"},
		},
	}).State
	approval := apply(t, tools, ApprovalRequired{
		RequestID: "approval-a", CallID: "call-a",
	}).State
	input := apply(t, sampling, InputRequired{RequestID: "input-1"}).State
	committing := apply(t, sampling, TerminalRequested{
		FailureMessage: "fixture failure",
	}).State

	testCases := []struct {
		name    string
		state   State
		command Command
	}{
		{
			name:  "tool result before proposal",
			state: sampling,
			command: ToolResultReceived{
				CallID: "call-a",
			},
		},
		{
			name:  "approval before tool",
			state: sampling,
			command: ApprovalRequired{
				RequestID: "approval-a", CallID: "call-a",
			},
		},
		{
			name:    "approval result before request",
			state:   tools,
			command: ApprovalResolved{RequestID: "approval-a"},
		},
		{
			name:  "duplicate approval request",
			state: approval,
			command: ApprovalRequired{
				RequestID: "approval-a", CallID: "call-a",
			},
		},
		{
			name:    "wrong input result",
			state:   input,
			command: InputResolved{RequestID: "input-late"},
		},
		{
			name:    "finish before terminal request",
			state:   sampling,
			command: FinishTerminal{},
		},
		{
			name:  "second terminal request",
			state: committing,
			command: TerminalRequested{
				FailureMessage: "second terminal",
			},
		},
		{
			name:    "late model text while committing",
			state:   committing,
			command: ModelTextReceived{Text: "late"},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			assertRejectedWithoutMutation(
				t,
				testCase.state,
				testCase.command,
			)
		})
	}

	left := closeToolCalls(t, tools, []string{"call-a", "call-b"})
	right := closeToolCalls(t, tools, []string{"call-b", "call-a"})
	assertEquivalentState(t, left, right)

	parallelApprovals := apply(t, approval, ApprovalRequired{
		RequestID: "approval-b", CallID: "call-b",
	}).State
	left = resolveApprovals(
		t,
		parallelApprovals,
		[]string{"approval-a", "approval-b"},
	)
	right = resolveApprovals(
		t,
		parallelApprovals,
		[]string{"approval-b", "approval-a"},
	)
	assertEquivalentState(t, left, right)
}

func TestStateGraphWaitingRequestIdentitySurvivesReplay(t *testing.T) {
	testCases := []struct {
		name      string
		state     State
		requestID string
		resolve   func(State) State
	}{
		{
			name: "approval",
			state: apply(
				t,
				apply(t, startSampling(t, protocol.TurnIntentAnswer), ToolCallsProposed{
					Calls: []ToolCallState{{ID: "call-1", Name: "write"}},
				}).State,
				ApprovalRequired{
					RequestID: "approval-stable", CallID: "call-1",
				},
			).State,
			requestID: "approval-stable",
			resolve: func(state State) State {
				return apply(t, state, ApprovalResolved{
					RequestID: "approval-stable",
				}).State
			},
		},
		{
			name: "input",
			state: apply(t, startSampling(t, protocol.TurnIntentAnswer), InputRequired{
				RequestID: "input-stable",
			}).State,
			requestID: "input-stable",
			resolve: func(state State) State {
				return apply(t, state, InputResolved{
					RequestID: "input-stable",
				}).State
			},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			data, err := json.Marshal(testCase.state)
			if err != nil {
				t.Fatal(err)
			}
			var restored State
			if err := json.Unmarshal(data, &restored); err != nil {
				t.Fatal(err)
			}
			if err := Validate(restored); err != nil {
				t.Fatalf("restored wait is invalid: %v", err)
			}
			if !stateHasRequest(restored, testCase.requestID) {
				t.Fatalf(
					"restored state lost request %q: %+v",
					testCase.requestID,
					restored,
				)
			}
			resolved := testCase.resolve(restored)
			if stateHasRequest(resolved, testCase.requestID) {
				t.Fatalf(
					"resolved state retained request %q: %+v",
					testCase.requestID,
					resolved,
				)
			}
		})
	}
}

func TestStateGraphWaitingRequestIdentitySurvivesCoordinatorRestore(
	t *testing.T,
) {
	testCases := []struct {
		name      string
		commands  []Command
		requestID string
	}{
		{
			name: "approval",
			commands: []Command{
				StartTurn{},
				PreparationFinished{},
				ToolCallsProposed{Calls: []ToolCallState{{
					ID: "call-1", Name: "write",
				}}},
				ApprovalRequired{
					RequestID: "approval-durable", CallID: "call-1",
				},
			},
			requestID: "approval-durable",
		},
		{
			name: "input",
			commands: []Command{
				StartTurn{},
				PreparationFinished{},
				InputRequired{RequestID: "input-durable"},
			},
			requestID: "input-durable",
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			store := NewMemoryTerminalEnvelopeStore(nil, nil)
			coordinator := newDeferredTestCoordinator(
				t,
				store,
				NewDurableEffectDispatcher(),
			)
			for _, command := range testCase.commands {
				if err := coordinator.Submit(
					t.Context(),
					command,
				); err != nil {
					t.Fatalf("submit %T: %v", command, err)
				}
			}
			before := coordinator.Snapshot()
			restored, err := RestoreTurnCoordinator(
				t.Context(),
				"turn-test",
				store,
				NewDurableEffectDispatcher(),
			)
			if err != nil {
				t.Fatal(err)
			}
			after := restored.Snapshot()
			assertEquivalentState(t, before, after)
			if !stateHasRequest(after, testCase.requestID) {
				t.Fatalf(
					"coordinator restore lost request %q: %+v",
					testCase.requestID,
					after,
				)
			}
		})
	}
}

func closeToolCalls(t *testing.T, state State, callIDs []string) State {
	t.Helper()
	for _, callID := range callIDs {
		state = apply(t, state, ToolResultReceived{CallID: callID}).State
	}
	return state
}

func resolveApprovals(
	t *testing.T,
	state State,
	requestIDs []string,
) State {
	t.Helper()
	for _, requestID := range requestIDs {
		state = apply(t, state, ApprovalResolved{
			RequestID: requestID,
		}).State
	}
	return state
}

func assertEquivalentState(t *testing.T, left, right State) {
	t.Helper()
	leftDigest, err := Digest(left)
	if err != nil {
		t.Fatal(err)
	}
	rightDigest, err := Digest(right)
	if err != nil {
		t.Fatal(err)
	}
	if leftDigest != rightDigest {
		t.Fatalf(
			"independent command order changed state: %s != %s\nleft=%+v\nright=%+v",
			leftDigest,
			rightDigest,
			left,
			right,
		)
	}
}

func assertRejectedWithoutMutation(
	t *testing.T,
	state State,
	command Command,
) {
	t.Helper()
	before := cloneState(state)
	beforeDigest, err := Digest(before)
	if err != nil {
		t.Fatal(err)
	}
	if _, applyErr := (Reducer{}).Apply(state, command); !errors.Is(
		applyErr,
		ErrIllegalTransition,
	) {
		t.Fatalf(
			"command %T error = %v, want illegal transition",
			command,
			applyErr,
		)
	}
	afterDigest, err := Digest(state)
	if err != nil {
		t.Fatal(err)
	}
	if beforeDigest != afterDigest || !reflect.DeepEqual(before, state) {
		t.Fatalf("rejected command %T mutated state", command)
	}
}

func assertTerminalRejectsEveryLateCommand(t *testing.T, state State) {
	t.Helper()
	commands := []Command{
		StartTurn{},
		PreparationFinished{},
		ModelSampleRequested{SampleID: "late-sample"},
		ModelSampleStarted{SampleID: "late-sample", Attempt: 1},
		ModelSampleFinished{SampleID: "late-sample"},
		ModelTextReceived{Text: "late"},
		ToolCallsProposed{Calls: []ToolCallState{{
			ID: "late-call", Name: "late",
		}}},
		ApprovalRequired{
			RequestID: "late-approval", CallID: "late-call",
		},
		ApprovalResolved{RequestID: "late-approval"},
		InputRequired{RequestID: "late-input"},
		InputResolved{RequestID: "late-input"},
		ToolResultReceived{CallID: "late-call"},
		VerificationRetired{},
		VerificationRetired{},
		CompletionEvaluated{Candidate: CompletionCandidate{
			CompletionCall: "late-completion",
		}},
		CancelRequested{Reason: "late cancel"},
		TerminalRequested{FailureMessage: "late terminal"},
		JournalFinalized{Status: JournalRolledBack},
		FinishTerminal{},
	}
	for _, command := range commands {
		assertRejectedWithoutMutation(t, state, command)
	}
}

func stateHasRequest(state State, requestID string) bool {
	if state.PendingInput != nil &&
		state.PendingInput.RequestID == requestID {
		return true
	}
	_, ok := state.PendingApprovals[requestID]
	return ok
}
