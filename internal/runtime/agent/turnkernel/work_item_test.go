package turnkernel

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestRepeatedToolCallsProposedIncrementsNoProgress(t *testing.T) {
	state := startSampling(t, protocol.TurnIntentAnswer)
	state.Policy.Convergence = ConvergencePolicyForStepLimit(64)
	signature := FormatProgressSignature(state, 0, false)
	state = apply(t, state, ObserveProgress{
		Signature: signature, CompletedSamples: 0,
	}).State
	for index := range 3 {
		state = apply(t, state, ToolCallsProposed{
			Calls: []ToolCallState{{
				ID:        fmt.Sprintf("echo-%d", index),
				Name:      "echo",
				Arguments: `{"text":"read"}`,
			}},
		}).State
		state = apply(t, state, ToolResultReceived{
			CallID: fmt.Sprintf("echo-%d", index),
		}).State
		state = apply(t, state, ObserveProgress{
			Signature:        FormatProgressSignature(state, 0, false),
			CompletedSamples: uint32(index + 1),
		}).State
	}
	if state.Progress.PendingIdentity == "" {
		t.Fatal("pending tool identity was not recorded")
	}
	if state.Progress.NoProgressSamples != 3 {
		t.Fatalf("repeated calls = %+v, want no_progress=3", state.Progress)
	}
}

func TestFormatToolCallsIdentityIgnoresJSONWhitespace(t *testing.T) {
	left := FormatToolCallsIdentity([]ToolCallState{{
		Name: "exec_command", Arguments: `{"command":"node --test","cwd":"."}`,
	}})
	right := FormatToolCallsIdentity([]ToolCallState{{
		Name: "exec_command", Arguments: `{ "cwd" : ".", "command" : "node --test" }`,
	}})
	if left == "" || left != right {
		t.Fatalf("canonical identity mismatch: %q vs %q", left, right)
	}
	other := FormatToolCallsIdentity([]ToolCallState{{
		Name: "exec_command", Arguments: `{"command":"node --test --test-name=foo"}`,
	}})
	if other == left {
		t.Fatal("distinct arguments shared identity")
	}
}

func TestSecondFullReadDoesNotChangeWorkItemSignature(t *testing.T) {
	state := startSampling(t, protocol.TurnIntentAnswer)
	state = apply(t, state, BindWorkItem{
		Goal: "fix the deadlock",
		KnownReads: map[string]WorkItemRead{
			"socket_transport.cpp": {Window: "full"},
		},
	}).State
	before := FormatProgressSignature(state, 0, false)
	state = apply(t, state, ToolCallsProposed{
		Calls: []ToolCallState{{
			ID: "read-2", Name: "file_read",
			Arguments: `{"path":"socket_transport.cpp"}`,
		}},
	}).State
	state = apply(t, state, ToolResultReceived{
		CallID: "read-2",
		Observation: WorkItemObservation{
			ReadPath:   "socket_transport.cpp",
			ReadWindow: "full",
		},
	}).State
	if after := FormatProgressSignature(state, 0, false); after != before {
		t.Fatalf("second full read renewed signature: before=%q after=%q", before, after)
	}
}

func TestSamePathEditDoesNotRenewWorkItemSignature(t *testing.T) {
	state := startSampling(t, protocol.TurnIntentWorkspaceChange)
	state = apply(t, state, ToolCallsProposed{
		Calls: []ToolCallState{{ID: "edit-1", Name: "file_edit"}},
	}).State
	state = apply(t, state, ToolResultReceived{
		CallID:  "edit-1",
		Changes: []ObservedChange{{Path: "socket_transport.cpp", Kind: "modified"}},
	}).State
	first := FormatProgressSignature(state, 0, false)
	if !strings.Contains(first, "edits=socket_transport.cpp") {
		t.Fatalf("first edit missing path set: %q", first)
	}
	state = apply(t, state, ToolCallsProposed{
		Calls: []ToolCallState{{ID: "edit-2", Name: "file_edit"}},
	}).State
	state = apply(t, state, ToolResultReceived{
		CallID:  "edit-2",
		Changes: []ObservedChange{{Path: "socket_transport.cpp", Kind: "modified"}},
	}).State
	if after := FormatProgressSignature(state, 0, false); after != first {
		t.Fatalf("same-path edit renewed signature: first=%q after=%q", first, after)
	}
	if state.MutationRevision != 2 {
		t.Fatalf("mutation revision = %d, want 2", state.MutationRevision)
	}
	state = apply(t, state, ObserveProgress{
		Signature: first, CompletedSamples: 0,
	}).State
	stale := apply(t, state, ObserveProgress{
		Signature: first, CompletedSamples: 1,
	}).State
	if stale.Progress.NoProgressSamples != 1 {
		t.Fatalf("same-path edit cleared no-progress: %+v", stale.Progress)
	}
}

func TestAnswerTurnKnownWorkItemFinishOnlyAtImplementLease(t *testing.T) {
	state := startSampling(t, protocol.TurnIntentAnswer)
	state.Policy.Convergence = ConvergencePolicyForStepLimit(64)
	state.Policy.ImplementNoProgressSamples = 6
	state = apply(t, state, BindWorkItem{
		Goal: "再试一下能不能跑通 socket_transport_test？",
		KnownReads: map[string]WorkItemRead{
			"socket_transport.cpp": {Window: "full"},
		},
	}).State
	signature := FormatProgressSignature(state, 0, false)
	state = apply(t, state, ObserveProgress{
		Signature:        signature,
		SampleIdentity:   "exec_command\nnode --test",
		CompletedSamples: 0,
	}).State
	for _, test := range []struct {
		samples uint32
		want    ProgressStage
	}{
		{samples: 2, want: ProgressStageNone},
		{samples: 3, want: ProgressStageConverge},
		{samples: 6, want: ProgressStageFinishOnly},
	} {
		state = apply(t, state, ObserveProgress{
			Signature:        signature,
			SampleIdentity:   "exec_command\nnode --test",
			CompletedSamples: test.samples,
		}).State
		if state.Progress.Stage != test.want ||
			state.Progress.NoProgressSamples != test.samples {
			t.Fatalf(
				"samples=%d progress=%+v, want stage=%s",
				test.samples,
				state.Progress,
				test.want,
			)
		}
	}
}

func TestDistinctToolIdentityDoesNotConsumeImplementLease(t *testing.T) {
	state := startSampling(t, protocol.TurnIntentAnswer)
	state.Policy.Convergence = ConvergencePolicyForStepLimit(64)
	state.Policy.ImplementNoProgressSamples = 6
	state = apply(t, state, BindWorkItem{
		Goal: "enrich the chapter",
		KnownReads: map[string]WorkItemRead{
			"readme.md": {Window: "full"},
		},
	}).State
	signature := FormatProgressSignature(state, 0, false)
	state = apply(t, state, ObserveProgress{
		Signature: signature, CompletedSamples: 0,
	}).State
	for samples := uint32(1); samples <= 8; samples++ {
		state = apply(t, state, ObserveProgress{
			Signature:        signature,
			SampleIdentity:   "exec_command\ncheck-" + strconv.FormatUint(uint64(samples), 10),
			CompletedSamples: samples,
		}).State
		if state.Progress.Stage != ProgressStageNone ||
			state.Progress.NoProgressSamples != samples ||
			state.Progress.StallKind != ProgressStallCycle {
			t.Fatalf(
				"distinct tool identity consumed short lease or reset long clock: samples=%d progress=%+v",
				samples,
				state.Progress,
			)
		}
	}
}

func TestAlternatingToolIdentityUsesMaxStepsLease(t *testing.T) {
	state := startSampling(t, protocol.TurnIntentAnswer)
	state.Policy.Convergence = ConvergencePolicyForStepLimit(64)
	state.Policy.ImplementNoProgressSamples = 6
	state = apply(t, state, BindWorkItem{
		Goal: "search the chapter",
		KnownReads: map[string]WorkItemRead{
			"readme.md": {Window: "full"},
		},
	}).State
	signature := FormatProgressSignature(state, 0, false)
	state = apply(t, state, ObserveProgress{
		Signature: signature, CompletedSamples: 0,
	}).State
	identities := []string{
		"search_text\nquery=A",
		"search_text\nquery=B",
		"search_text\nquery=A",
		"search_text\nquery=B",
		"search_text\nquery=A",
		"search_text\nquery=B",
	}
	for index, identity := range identities {
		samples := uint32(index + 1)
		state = apply(t, state, ObserveProgress{
			Signature:        signature,
			SampleIdentity:   identity,
			CompletedSamples: samples,
		}).State
		if state.Progress.Stage != ProgressStageNone ||
			state.Progress.NoProgressSamples != samples ||
			state.Progress.StallKind != ProgressStallCycle {
			t.Fatalf(
				"A/B cycle used short lease: samples=%d progress=%+v",
				samples,
				state.Progress,
			)
		}
	}
}

func TestNewResultDigestRenewsNoProgress(t *testing.T) {
	state := startSampling(t, protocol.TurnIntentAnswer)
	state.Policy.Convergence = ConvergencePolicyForStepLimit(64)
	state.Policy.ImplementNoProgressSamples = 6
	signature := FormatProgressSignature(state, 0, false)
	state = apply(t, state, ObserveProgress{
		Signature:        signature,
		SampleIdentity:   "exec_command\nnode --test",
		ResultDigest:     "ok;content=sha256:one",
		CompletedSamples: 0,
	}).State
	state = apply(t, state, ObserveProgress{
		Signature:        signature,
		SampleIdentity:   "exec_command\nnode --test --name=other",
		ResultDigest:     "ok;content=sha256:two",
		CompletedSamples: 3,
	}).State
	if state.Progress.NoProgressSamples != 0 ||
		state.Progress.Stage != ProgressStageNone ||
		state.Progress.StallKind != ProgressStallNone {
		t.Fatalf("new result digest did not renew: %+v", state.Progress)
	}
}

func TestSeenWorkStateDoesNotResetOnReturn(t *testing.T) {
	state := startSampling(t, protocol.TurnIntentWorkspaceChange)
	state.Policy.Convergence = ConvergencePolicyForStepLimit(64)
	state.Policy.ImplementNoProgressSamples = 6
	state = apply(t, state, BindWorkItem{
		Goal: "toggle the flag",
		KnownEdits: map[string]WorkItemEdit{
			"flag.go": {ContentDigest: "sha256:a"},
		},
	}).State
	first := FormatProgressSignature(state, 0, false)
	state = apply(t, state, ObserveProgress{
		Signature:        first,
		SampleIdentity:   "file_edit\nflag.go",
		ResultDigest:     "ok;read=sha256:a",
		CompletedSamples: 0,
	}).State
	state.WorkItem.KnownEdits["flag.go"] = WorkItemEdit{ContentDigest: "sha256:b"}
	second := FormatProgressSignature(state, 0, false)
	state = apply(t, state, ObserveProgress{
		Signature:        second,
		SampleIdentity:   "file_edit\nflag.go",
		ResultDigest:     "ok;read=sha256:b",
		CompletedSamples: 1,
	}).State
	if state.Progress.NoProgressSamples != 0 {
		t.Fatalf("new content version did not renew: %+v", state.Progress)
	}
	state.WorkItem.KnownEdits["flag.go"] = WorkItemEdit{ContentDigest: "sha256:a"}
	state = apply(t, state, ObserveProgress{
		Signature:        first,
		SampleIdentity:   "file_edit\nflag.go",
		ResultDigest:     "ok;read=sha256:a",
		CompletedSamples: 2,
	}).State
	if state.Progress.NoProgressSamples != 1 ||
		state.Progress.StallKind != ProgressStallIdenticalCall {
		t.Fatalf("returned content version reset clock: %+v", state.Progress)
	}
}

func TestPaginationReadRenewsObservation(t *testing.T) {
	state := startSampling(t, protocol.TurnIntentAnswer)
	state.Policy.Convergence = ConvergencePolicyForStepLimit(64)
	state = apply(t, state, BindWorkItem{
		Goal: "read the transport",
		KnownReads: map[string]WorkItemRead{
			"socket_transport.cpp": {Window: "40", StartLine: 40, EndLine: 80},
		},
	}).State
	signature := FormatProgressSignature(state, 0, false)
	state = apply(t, state, ObserveProgress{
		Signature:        signature,
		SampleIdentity:   "file_read\nsocket_transport.cpp",
		ResultDigest:     "ok;read=sha256:page1",
		CompletedSamples: 0,
	}).State
	state.WorkItem.KnownReads["socket_transport.cpp"] = WorkItemRead{
		Window: "80", StartLine: 80, EndLine: 120,
	}
	state = apply(t, state, ObserveProgress{
		Signature:        signature,
		SampleIdentity:   "file_read\nsocket_transport.cpp",
		ResultDigest:     "ok;read=sha256:page2",
		CompletedSamples: 2,
	}).State
	if state.Progress.NoProgressSamples != 0 {
		t.Fatalf("pagination did not renew: %+v", state.Progress)
	}
}

func TestKnownWorkItemWithoutRepeatedCallsKeepsMaxStepsLease(t *testing.T) {
	state := startSampling(t, protocol.TurnIntentAnswer)
	state.Policy.Convergence = ConvergencePolicyForStepLimit(64)
	state.Policy.ImplementNoProgressSamples = 6
	state = apply(t, state, BindWorkItem{
		Goal: "read then write",
		KnownReads: map[string]WorkItemRead{
			"readme.md": {Window: "full"},
		},
	}).State
	signature := FormatProgressSignature(state, 0, false)
	state = apply(t, state, ObserveProgress{
		Signature: signature, CompletedSamples: 0,
	}).State
	state = apply(t, state, ObserveProgress{
		Signature: signature, CompletedSamples: 6,
	}).State
	if state.Progress.Stage != ProgressStageNone {
		t.Fatalf(
			"known work item without repeated calls used implement lease: %+v",
			state.Progress,
		)
	}
}

func TestObserveWorkItemResultRecordsReadAndSession(t *testing.T) {
	observation := ObserveWorkItemResult(
		ToolCallState{
			Name:      "file_read",
			Arguments: `{"path":"socket_transport.cpp","start_line":40}`,
		},
		tool.Result{Outcome: &tool.Outcome{Facts: &tool.OutcomeFacts{
			WorkspaceRead: &tool.WorkspaceReadFact{
				Path: "socket_transport.cpp", Digest: "sha256:abc",
			},
		}}},
	)
	if observation.ReadPath != "socket_transport.cpp" ||
		observation.ReadWindow != "40" ||
		observation.ContentDigest != "sha256:abc" {
		t.Fatalf("read observation = %+v", observation)
	}
	running := ObserveWorkItemResult(
		ToolCallState{Name: "exec_command"},
		tool.Result{Outcome: &tool.Outcome{Facts: &tool.OutcomeFacts{
			ProcessSession: &tool.ProcessSessionFact{
				SessionID: "sess-1", Running: true,
			},
		}}},
	)
	if running.OpenSession != "sess-1" || running.CloseSession != "" {
		t.Fatalf("running session = %+v", running)
	}
}

func TestBindWorkItemSeedsContinueKnownReads(t *testing.T) {
	kernel, err := NewRuntimeKernel(
		KernelIdentity{
			TurnID:          "continue-work-item",
			ProfileRevision: 1,
			Goal:            "你上一轮socket_transport_test测试时死锁了",
			WorkItem: WorkItem{
				KnownReads: map[string]WorkItemRead{
					"socket_transport.cpp": {Window: "full"},
				},
			},
		},
		protocol.TurnIntentAnswer,
		"act",
		&protocol.TurnRecoveryContext{
			Action:       protocol.TurnRecoveryContinue,
			SourceTurnID: "turn-source",
		},
		false,
		nil,
		nil,
		nil,
		nil,
		nil,
		DefaultPolicy(),
		NewEphemeralCoordinatorRuntime(),
	)
	if err != nil {
		t.Fatal(err)
	}
	item := kernel.WorkItem()
	if item.GoalDigest != GoalDigest("你上一轮socket_transport_test测试时死锁了") {
		t.Fatalf("goal digest = %q", item.GoalDigest)
	}
	if _, ok := item.KnownReads["socket_transport.cpp"]; !ok {
		t.Fatalf("known reads = %+v", item.KnownReads)
	}
	if !kernel.RecoveryContinue() {
		t.Fatal("continue relation was not bound")
	}
	before := kernel.ProgressSignature(0, false)
	call := provider.ToolCall{
		ID: "read-again", Name: "file_read",
		Arguments: `{"path":"socket_transport.cpp"}`,
	}
	if err := kernel.StartTools([]provider.ToolCall{call}); err != nil {
		t.Fatal(err)
	}
	if err := kernel.StartTool(call.ID); err != nil {
		t.Fatal(err)
	}
	if err := kernel.CloseTool(call, tool.Result{
		Outcome: &tool.Outcome{Facts: &tool.OutcomeFacts{
			WorkspaceRead: &tool.WorkspaceReadFact{Path: "socket_transport.cpp"},
		}},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if after := kernel.ProgressSignature(0, false); after != before {
		t.Fatalf("seeded re-read renewed signature: before=%q after=%q", before, after)
	}
}

func TestImplementLeaseExhaustsAtLeasePlusRepairReserve(t *testing.T) {
	state := startSampling(t, protocol.TurnIntentAnswer)
	state.Policy.Convergence = ConvergencePolicyForStepLimit(64)
	state.Policy.CompletionRepairLimit = 2
	state.Policy.WorkspaceRepairLimit = 1
	state.Policy.DeclarationRepairLimit = 1
	state.Policy.VerificationRepairLimit = 1
	state.Policy.ImplementNoProgressSamples = 6
	state = apply(t, state, BindWorkItem{
		Goal: "settle the transport regression",
		KnownReads: map[string]WorkItemRead{
			"socket_transport.cpp": {Window: "full"},
		},
	}).State
	signature := FormatProgressSignature(state, 0, false)
	state = apply(t, state, ObserveProgress{
		Signature:        signature,
		SampleIdentity:   "file_read\nsocket_transport.cpp",
		CompletedSamples: 0,
	}).State
	// Between finish-only and lease+repair-reserve the stage stays
	// finish-only instead of trailing behind the 69-sample step lease.
	for _, test := range []struct {
		samples uint32
		want    ProgressStage
	}{
		{samples: 6, want: ProgressStageFinishOnly},
		{samples: 10, want: ProgressStageFinishOnly},
		{samples: 11, want: ProgressStageExhausted},
	} {
		state = apply(t, state, ObserveProgress{
			Signature:        signature,
			SampleIdentity:   "file_read\nsocket_transport.cpp",
			CompletedSamples: test.samples,
		}).State
		if state.Progress.Stage != test.want {
			t.Fatalf(
				"samples=%d stage=%s, want %s",
				test.samples, state.Progress.Stage, test.want,
			)
		}
	}
	if state.Convergence == nil ||
		state.Convergence.Cause != ConvergenceNoProgress {
		t.Fatalf("exhausted convergence = %+v", state.Convergence)
	}
}

func TestNoProgressConvergenceKeepsRejectedCompletionSummary(t *testing.T) {
	state := startSampling(t, protocol.TurnIntentAnswer)
	state.Policy.Convergence = ConvergencePolicyForStepLimit(64)
	state.Policy.ImplementNoProgressSamples = 6
	state.Completion = &CompletionDecision{
		Accepted: false,
		Summary:  "已全面扩充 foundations",
		Reason:   "incomplete_declaration",
	}
	signature := FormatProgressSignature(state, 0, false)
	state = apply(t, state, ObserveProgress{
		Signature:        signature,
		SampleIdentity:   "exec_command\nnode --test",
		CompletedSamples: 0,
	}).State
	state = apply(t, state, ObserveProgress{
		Signature:        signature,
		SampleIdentity:   "exec_command\nnode --test",
		CompletedSamples: 11,
	}).State
	if state.Convergence == nil ||
		state.Convergence.Cause != ConvergenceNoProgress ||
		state.Convergence.Summary != "已全面扩充 foundations" ||
		len(state.Convergence.PendingActions) != 1 {
		t.Fatalf("convergence = %+v", state.Convergence)
	}
}

func TestImplementLeaseWithoutRepairReserveExhaustsAtLeasePlusOne(t *testing.T) {
	state := startSampling(t, protocol.TurnIntentAnswer)
	state.Policy.Convergence = ConvergencePolicyForStepLimit(64)
	state.Policy.CompletionRepairLimit = 0
	state.Policy.WorkspaceRepairLimit = 0
	state.Policy.DeclarationRepairLimit = 0
	state.Policy.VerificationRepairLimit = 0
	state.Policy.ImplementNoProgressSamples = 6
	state = apply(t, state, BindWorkItem{
		Goal: "settle the transport regression",
		KnownReads: map[string]WorkItemRead{
			"socket_transport.cpp": {Window: "full"},
		},
	}).State
	signature := FormatProgressSignature(state, 0, false)
	state = apply(t, state, ObserveProgress{
		Signature:        signature,
		SampleIdentity:   "file_read\nsocket_transport.cpp",
		CompletedSamples: 0,
	}).State
	state = apply(t, state, ObserveProgress{
		Signature:        signature,
		SampleIdentity:   "file_read\nsocket_transport.cpp",
		CompletedSamples: 6,
	}).State
	if state.Progress.Stage != ProgressStageFinishOnly {
		t.Fatalf("stage = %s, want finish_only", state.Progress.Stage)
	}
	state = apply(t, state, ObserveProgress{
		Signature:        signature,
		SampleIdentity:   "file_read\nsocket_transport.cpp",
		CompletedSamples: 7,
	}).State
	if state.Progress.Stage != ProgressStageExhausted {
		t.Fatalf("stage = %s, want exhausted", state.Progress.Stage)
	}
}
