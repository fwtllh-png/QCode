package turnkernel

import (
	"encoding/json"
	"fmt"
	"testing"

	adaptercontent "github.com/fwtllh-png/QCode/internal/adapter/content"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/observability/diagnostics"
	"github.com/fwtllh-png/QCode/internal/observability/verify"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestNoisyFailedCommandsExhaustProgressLease(t *testing.T) {
	state := startSampling(t, protocol.TurnIntentWorkspaceChange)
	state.Policy.Convergence = ConvergencePolicyForStepLimit(64)
	state.Policy.ImplementNoProgressSamples = 6
	for index := 0; index <= 6; index++ {
		call := ToolCallState{ID: fmt.Sprintf("attempt-%d", index), Name: "exec_command", Arguments: `{"command":"go test ./..."}`}
		result := tool.Result{
			IsError: true,
			Content: fmt.Sprintf("FAIL TestParser at 12:00:%02d in /tmp/run-%d (%.2fs)", index, index, float64(index)),
			Handle:  fmt.Sprintf("result-%d", index),
			Outcome: &tool.Outcome{Facts: &tool.OutcomeFacts{
				ProcessSession: &tool.ProcessSessionFact{SessionID: fmt.Sprintf("process-%d", index), Cursor: uint64(index * 100), ExitCode: 1},
				Verification:   &verify.Evidence{Kind: "test", Status: verify.StatusFailed, InputDigest: "same-input", CoveredPaths: []string{"parser.go"}, ExitCode: 1, CallID: call.ID},
			}},
		}
		state = apply(t, state, ToolCallsProposed{Calls: []ToolCallState{call}}).State
		state = apply(t, state, ToolResultReceived{CallID: call.ID, IsError: true, Observation: ObserveWorkItemResult(call, result)}).State
		state = apply(t, state, ObserveProgress{Signature: FormatProgressSignature(state, 0, false), CompletedSamples: uint32(index + 1)}).State
		if index == 3 {
			// Recovery must preserve the semantic observation clock.
			encoded, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			var recovered State
			if err = json.Unmarshal(encoded, &recovered); err != nil {
				t.Fatal(err)
			}
			state = recovered
		}
	}
	if state.Progress.NoProgressSamples != 6 || state.Progress.Stage != ProgressStageFinishOnly {
		t.Fatalf("noisy retries renewed the lease: %+v", state.Progress)
	}
}

func TestResultObservationIgnoresPresentationAndAttemptIdentity(t *testing.T) {
	for _, isError := range []bool{false, true} {
		first := tool.Result{Admission: &adaptercontent.AdmissionReceipt{Digest: "first-output"}, IsError: isError, Content: "elapsed=1 /tmp/first", Handle: "one", Outcome: &tool.Outcome{Facts: &tool.OutcomeFacts{
			ProcessSession: &tool.ProcessSessionFact{SessionID: "one", Cursor: 10, ExitCode: 1},
			Diagnostics:    []diagnostics.Receipt{{Path: "a.go", Status: "failed", Message: "elapsed=1", Diagnostics: []diagnostics.Diagnostic{{Path: "a.go", Code: "E1", Message: "/tmp/first"}}}},
			Verification:   &verify.Evidence{Kind: "test", Status: verify.StatusFailed, InputDigest: "input", CallID: "one", CommandDigest: "one", MutationRevision: 1},
		}}}
		second := first
		second.Content = "elapsed=2 /tmp/second"
		second.Handle = "two"
		second.Admission = &adaptercontent.AdmissionReceipt{Digest: "second-output"}
		second.Outcome = tool.CloneOutcome(first.Outcome)
		facts := second.Outcome.Facts
		facts.ProcessSession.SessionID = "two"
		facts.ProcessSession.Cursor = 200
		facts.Diagnostics[0].Message = "elapsed=2"
		facts.Diagnostics[0].Diagnostics[0].Message = "/tmp/second"
		facts.Verification.CallID = "two"
		facts.Verification.CommandDigest = "two"
		facts.Verification.MutationRevision = 2
		if ResultObservationDigest(first) != ResultObservationDigest(second) {
			t.Fatal("presentation or attempt identity counted as progress")
		}
	}
}

func TestStructuredResultChangesRenewProgress(t *testing.T) {
	for name, pair := range map[string][2]*tool.OutcomeFacts{
		"search location": {
			{Evidence: []tool.EvidenceHit{{Kind: "text", Path: "a.go", Line: 10}}},
			{Evidence: []tool.EvidenceHit{{Kind: "text", Path: "a.go", Line: 20}}},
		},
		"file version": {
			{WorkspaceRead: &tool.WorkspaceReadFact{Path: "a.go", Digest: "old"}},
			{WorkspaceRead: &tool.WorkspaceReadFact{Path: "a.go", Digest: "new"}},
		},
		"edit version": {
			{WorkspaceChanges: []tool.WorkspaceChange{{Path: "a.go", Kind: tool.WorkspaceModified, AfterDigest: "old"}}},
			{WorkspaceChanges: []tool.WorkspaceChange{{Path: "a.go", Kind: tool.WorkspaceModified, AfterDigest: "new"}}},
		},
		"diagnostic code": {
			{Diagnostics: []diagnostics.Receipt{{Path: "a.go", Diagnostics: []diagnostics.Diagnostic{{Path: "a.go", Code: "E1"}}}}},
			{Diagnostics: []diagnostics.Receipt{{Path: "a.go", Diagnostics: []diagnostics.Diagnostic{{Path: "a.go", Code: "E2"}}}}},
		},
		"verification passed": {
			{Verification: &verify.Evidence{Kind: "test", Status: verify.StatusFailed, InputDigest: "input", ExitCode: 1}},
			{Verification: &verify.Evidence{Kind: "test", Status: verify.StatusPassed, InputDigest: "input"}},
		},
		"process completed": {
			{ProcessSession: &tool.ProcessSessionFact{Running: true}},
			{ProcessSession: &tool.ProcessSessionFact{ExitCode: 0}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			before := ResultObservationDigest(tool.Result{Content: "unchanged", Outcome: &tool.Outcome{Facts: pair[0]}})
			after := ResultObservationDigest(tool.Result{Content: "unchanged", Outcome: &tool.Outcome{Facts: pair[1]}})
			state := startSampling(t, protocol.TurnIntentWorkspaceChange)
			state.Policy.Convergence = ConvergencePolicyForStepLimit(64)
			signature := FormatProgressSignature(state, 0, false)
			state = apply(t, state, ObserveProgress{Signature: signature, ResultDigest: before}).State
			state = apply(t, state, ObserveProgress{Signature: signature, ResultDigest: before, CompletedSamples: 3}).State
			state = apply(t, state, ObserveProgress{Signature: signature, ResultDigest: after, CompletedSamples: 4}).State
			if state.Progress.NoProgressSamples != 0 {
				t.Fatalf("new fact did not renew: %+v", state.Progress)
			}
		})
	}
}

func TestResultObservationCanonicalizesFactSets(t *testing.T) {
	a := tool.EvidenceHit{Path: "a.go", Line: 1}
	b := tool.EvidenceHit{Path: "b.go", Line: 2}
	result := func(hits []tool.EvidenceHit) tool.Result {
		return tool.Result{Outcome: &tool.Outcome{Facts: &tool.OutcomeFacts{Evidence: hits}}}
	}
	if ResultObservationDigest(result([]tool.EvidenceHit{a, b})) != ResultObservationDigest(result([]tool.EvidenceHit{b, a, a})) {
		t.Fatal("fact order or duplicates renewed progress")
	}
	if FormatResultDigest([]string{"a", "b"}) != FormatResultDigest([]string{"b", "a", "a"}) {
		t.Fatal("batch duplicates renewed progress")
	}
}

func TestProcessAttemptIdentityDoesNotRenewWorkItem(t *testing.T) {
	state := startSampling(t, protocol.TurnIntentWorkspaceChange)
	state.WorkItem.Open.Sessions = []string{"attempt-one"}
	first := FormatProgressSignature(state, 0, false)
	state.WorkItem.Open.Sessions = []string{"attempt-two"}
	if first != FormatProgressSignature(state, 0, false) {
		t.Fatal("new process ID renewed signature")
	}
	state.WorkItem.Open.Sessions = nil
	if first == FormatProgressSignature(state, 0, false) {
		t.Fatal("process completion did not change signature")
	}
}
