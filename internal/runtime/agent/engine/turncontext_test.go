package engine

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	providerfixture "github.com/fwtllh-png/QCode/internal/adapter/provider/fixture"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	toolguard "github.com/fwtllh-png/QCode/internal/adapter/tool/guard"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/interact"
	"github.com/fwtllh-png/QCode/internal/observability/verify"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	"github.com/fwtllh-png/QCode/internal/security/controlmatrix"
	"github.com/fwtllh-png/QCode/internal/security/policy"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func TestTruthMaxBytesForCapacityUsesRouteAndSummaryLimits(t *testing.T) {
	for _, test := range []struct {
		hardInputTokens uint64
		summaryMaxBytes int
		want            int
	}{
		{hardInputTokens: 8_192, want: 8_192},
		{hardInputTokens: 2 << 20, want: 1 << 20},
		{hardInputTokens: 32_000, summaryMaxBytes: 8_192, want: 7_936},
	} {
		if got := truthMaxBytesForCapacity(
			test.hardInputTokens,
			test.summaryMaxBytes,
		); got != test.want {
			t.Fatalf("truthMaxBytesForCapacity(%d, %d) = %d, want %d",
				test.hardInputTokens, test.summaryMaxBytes, got, test.want)
		}
	}
}

func TestSnapshotTurnSpecFreezesSessionInputs(t *testing.T) {
	security := policy.DefaultRuntime(policy.ModeOperate, policy.PermissionAuto)
	security.Repository = []policy.Rule{{
		Tool: "exec_command", Resource: "*", Action: policy.ActionAsk,
	}}
	registry := tool.NewRegistry(nil, nil)
	registry.SetSandboxBackend(turnContextBackend{})
	options := Options{ProviderConfig: ProviderConfig{Route: testRoute(t)}, ToolConfig: ToolConfig{Tools: registry}, SecurityConfig: SecurityConfig{Security: security, Workspace: "/tmp/ws"}}
	snapshot, err := SnapshotTurnSpec(
		options,
		TurnIdentity{SessionID: "session-1", TurnID: "turn-1", ProfileRevision: 7},
		TurnRequest{Prompt: "inspect", Intent: protocol.TurnIntentAnswer},
	)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Provider == "" || snapshot.Model == "" {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if snapshot.ModelMetadata == nil ||
		snapshot.ModelMetadata.Limits != string(model.ProvenanceFixture) ||
		snapshot.ModelMetadata.Capabilities != string(model.ProvenanceFixture) {
		t.Fatalf("model metadata provenance = %+v", snapshot.ModelMetadata)
	}
	if snapshot.Limits.Context.ContextTokens !=
		snapshot.Route.Model().Limits.ContextTokens ||
		snapshot.Limits.Context.OutputCeiling !=
			snapshot.Route.Model().Limits.MaxOutputTokens ||
		snapshot.Limits.Context.HardInputTokens !=
			snapshot.Route.Model().Limits.ContextTokens-
				snapshot.Route.Model().Limits.MaxOutputTokens {
		t.Fatalf("context capacity snapshot = %+v", snapshot.Limits.Context)
	}
	if snapshot.Identity.TurnID != "turn-1" ||
		snapshot.Identity.ProfileRevision != 7 ||
		snapshot.Request.Prompt != "inspect" ||
		snapshot.Catalog.Generation == 0 {
		t.Fatalf("identity/request/catalog not frozen: %+v", snapshot)
	}
	// Operate is act with wider permissions, not a purpose of its own.
	if snapshot.Purpose != model.PurposeAct {
		t.Fatalf("purpose = %q, want act", snapshot.Purpose)
	}
	if snapshot.Mode != policy.ModeOperate || snapshot.Posture != policy.PermissionAuto {
		t.Fatalf("mode/posture = %s/%s", snapshot.Mode, snapshot.Posture)
	}
	wantSandbox := "test/test/" + turnContextBackend{}.Capability().Effective.Identity()
	if snapshot.Workspace != "/tmp/ws" || snapshot.Sandbox != wantSandbox {
		t.Fatalf("workspace/sandbox = %q/%q", snapshot.Workspace, snapshot.Sandbox)
	}
	if snapshot.Policy == nil || snapshot.Policy == security {
		t.Fatal("snapshot must allocate a distinct sampling policy")
	}
	security.Mode = policy.ModePlan
	security.Permission = policy.PermissionNever
	if snapshot.Policy.Mode != policy.ModeOperate || snapshot.Policy.Permission != policy.PermissionAuto {
		t.Fatalf("clone mutated with session: %+v", snapshot.Policy)
	}
	if len(snapshot.Policy.Repository) != 1 {
		t.Fatalf("repository not copied: %+v", snapshot.Policy.Repository)
	}
	security.Repository[0].Action = policy.ActionDeny
	if snapshot.Policy.Repository[0].Action != policy.ActionAsk {
		t.Fatal("repository slice must be copied")
	}
}

func TestAnswerTurnProgressLeaseOmitsVerificationReserve(t *testing.T) {
	registry := tool.NewRegistry(nil, nil)
	options := Options{
		ProviderConfig: ProviderConfig{Route: testRoute(t), MaxSteps: 1},
		ToolConfig: ToolConfig{
			Tools: registry,
			Verify: VerifyOptions{
				Mode: "soft", MaxRepairSteps: 1, Runner: verifyStub{},
			},
		},
		SecurityConfig: SecurityConfig{Workspace: t.TempDir()},
	}
	answer, err := SnapshotTurnSpec(
		options,
		TurnIdentity{SessionID: "session-1", TurnID: "turn-1"},
		TurnRequest{Prompt: "inspect", Intent: protocol.TurnIntentAnswer},
	)
	if err != nil {
		t.Fatal(err)
	}
	wantAnswer := turnkernel.ConvergencePolicyForStepLimit(5)
	if answer.Kernel.Convergence != wantAnswer {
		t.Fatalf("answer lease = %+v, want %+v", answer.Kernel.Convergence, wantAnswer)
	}
	change, err := SnapshotTurnSpec(
		options,
		TurnIdentity{SessionID: "session-1", TurnID: "turn-2"},
		TurnRequest{Prompt: "edit", Intent: protocol.TurnIntentWorkspaceChange},
	)
	if err != nil {
		t.Fatal(err)
	}
	wantChange := turnkernel.ConvergencePolicyForStepLimit(6)
	if change.Kernel.Convergence != wantChange {
		t.Fatalf("workspace-change lease = %+v, want %+v", change.Kernel.Convergence, wantChange)
	}
}

type verifyStub struct{}

func (verifyStub) Verify(context.Context, verify.Request) (verify.Receipt, error) {
	return verify.Receipt{}, nil
}

func TestSnapshotTurnSpecLeavesStructuredTerminalOff(
	t *testing.T,
) {
	for _, test := range []struct {
		name      string
		inputHost *interact.Host
		intent    protocol.TurnIntent
		require   bool
		want      bool
	}{
		{name: "answer", inputHost: interact.NewHost(0), intent: protocol.TurnIntentAnswer, require: true, want: false},
		{name: "plan", intent: protocol.TurnIntentPlan, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec, err := SnapshotTurnSpec(
				Options{ProviderConfig: ProviderConfig{Route: testRoute(t)}, ToolConfig: ToolConfig{Tools: tool.NewRegistry(nil, nil),

					RequireCompletionDeclaration: test.require}, SecurityConfig: SecurityConfig{Security: policy.DefaultRuntime(
					policy.ModeAct,
					policy.PermissionBypass,
				)}, LifecycleConfig: LifecycleConfig{InputHost: test.inputHost},
				},
				TurnIdentity{TurnID: "turn-1"},
				TurnRequest{
					Prompt: "answer",
					Intent: test.intent,
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			if spec.Kernel.StructuredTerminalRequired != test.want {
				t.Fatalf(
					"structured terminal = %t, want %t",
					spec.Kernel.StructuredTerminalRequired,
					test.want,
				)
			}
			if spec.Kernel.CompletionRequired != test.require {
				t.Fatalf("completion required = %t", spec.Kernel.CompletionRequired)
			}
		})
	}
}

func TestSnapshotTurnSpecSelectsSkillsFromFrozenPrompt(t *testing.T) {
	options := Options{ProviderConfig: ProviderConfig{Route: testRoute(t)}, ToolConfig: ToolConfig{Tools: tool.NewRegistry(nil, nil)}, SecurityConfig: SecurityConfig{Security: policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass)}}
	options.TurnSnapshots.SkillSelection = func(
		query string,
	) ([]SkillSummary, SkillSelectionMetrics, error) {
		if query != "review this change" {
			t.Fatalf("selection query = %q", query)
		}
		return []SkillSummary{{
				Name: "code-review", Description: "Review code.",
				Handle: "skh_handle", PackageHandle: "skp_package",
				ResourceHandle: "skr_resource",
			}}, SkillSelectionMetrics{
				Method: "weighted_lexical_v1", CatalogSize: 1000,
				CandidateSize: 1, VisibleSize: 1, TokenSavings: 0.99,
				QueryTerms: 2, QueryTruncated: true, CandidateSetTruncated: true,
			}, nil
	}
	spec, err := SnapshotTurnSpec(
		options,
		TurnIdentity{SessionID: "session", TurnID: "turn", ProfileRevision: 1},
		TurnRequest{Prompt: "review this change"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Skills) != 1 || spec.Skills[0].Name != "code-review" ||
		spec.SkillSelection.CatalogSize != 1000 ||
		spec.SkillSelection.TokenSavings != 0.99 ||
		spec.SkillSelection.QueryTerms != 2 ||
		!spec.SkillSelection.QueryTruncated ||
		!spec.SkillSelection.CandidateSetTruncated {
		t.Fatalf("skill selection snapshot = %+v %+v", spec.Skills, spec.SkillSelection)
	}
}

func TestSnapshotTurnSpecDegradesMemoryReadFailure(t *testing.T) {
	options := Options{ProviderConfig: ProviderConfig{Route: testRoute(t)}, ContextConfig: ContextConfig{TurnSnapshots: TurnSnapshotSources{
		Memory: func(string) (MemorySnapshot, error) {
			return MemorySnapshot{}, errors.New("corrupt memory store")
		},
	}}, ToolConfig: ToolConfig{Tools: tool.NewRegistry(nil, nil)}, SecurityConfig: SecurityConfig{Security: policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass)},
	}
	spec, err := SnapshotTurnSpec(
		options,
		TurnIdentity{SessionID: "session", TurnID: "turn", ProfileRevision: 1},
		TurnRequest{Prompt: "continue"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Memory.FailureReason != "retrieval_failed" {
		t.Fatalf("memory snapshot=%+v", spec.Memory)
	}
	section, receipt, ok := (&Engine{}).memoryWorldSection(spec, 1)
	if !ok || section.ID != "" || !receipt.Truncated ||
		receipt.TruncationReason != "retrieval_failed" {
		t.Fatalf("section=%+v receipt=%+v ok=%t", section, receipt, ok)
	}
}

func TestRunForTurnIgnoresMidTurnPolicyMutation(t *testing.T) {
	security := policy.DefaultRuntime(policy.ModeAct, policy.PermissionSuggest)
	registry := tool.NewRegistry(nil, nil)
	if err := registry.Register(&turnWriteTool{}); err != nil {
		t.Fatal(err)
	}
	providerRuntime := &scriptedProvider{streams: []provider.Stream{
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventToolCallDelta, ToolCall: &provider.ToolCallFragment{
				ID: "w1", Name: "write", Arguments: `{"path":"a","value":"x"}`,
			}},
			{Type: provider.EventMessageStop},
		}},
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventToolCallDelta, ToolCall: &provider.ToolCallFragment{
				ID: "w2", Name: "write", Arguments: `{"path":"b","value":"y"}`,
			}},
			{Type: provider.EventMessageStop},
		}},
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventTextDelta, Text: "done"},
			{Type: provider.EventMessageStop},
		}},
	}}
	engine, err := newTestEngine(Options{ProviderConfig: ProviderConfig{Provider: providerRuntime, Route: testRoute(t),
		MaxOutputTokens: 128, MaxSteps: 8}, ToolConfig: ToolConfig{Tools: registry,

		Authorize: func(provider.ToolCall) bool { return true }}, SecurityConfig: SecurityConfig{Security: security, Workspace: t.TempDir()},
	})
	if err != nil {
		t.Fatal(err)
	}

	var (
		mu        sync.Mutex
		approvals int
		started   Event
	)
	done := make(chan error, 1)
	go func() {
		_, runErr := engine.RunForTurn(context.Background(), "freeze-1", "edit files", func(event Event) error {
			mu.Lock()
			defer mu.Unlock()
			if event.State == Preparing {
				started = event
			}
			if event.State == AwaitingApproval && event.Approval != nil {
				approvals++
				// Mid-turn host mutation: session would flip to bypass.
				security.Permission = policy.PermissionBypass
				security.Mode = policy.ModeOperate
				go func(requestID string) {
					time.Sleep(10 * time.Millisecond)
					_ = mustControl(t, engine).ResolveApproval(toolguard.ApprovalDecision{
						RequestID: requestID, Approved: true,
						Scope: policy.ApprovalOnce, ExpiresAt: time.Now().Add(time.Minute),
					})
				}(event.Approval.RequestID)
			}
			return nil
		})
		done <- runErr
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("turn timed out")
	}

	mu.Lock()
	defer mu.Unlock()
	if started.Mode != string(policy.ModeAct) || started.Posture != string(policy.PermissionSuggest) {
		t.Fatalf("started context = mode=%q posture=%q", started.Mode, started.Posture)
	}
	if started.ModelMetadata == nil ||
		started.ModelMetadata.Limits != string(model.ProvenanceFixture) {
		t.Fatalf("started model metadata = %+v", started.ModelMetadata)
	}
	// Two write tools under Suggest → two asks even after session flipped to bypass.
	if approvals != 2 {
		t.Fatalf("approvals = %d, want 2 (mid-turn bypass must not apply)", approvals)
	}
	if security.Permission != policy.PermissionBypass {
		t.Fatal("session permission should remain mutated for the next turn")
	}
}

func TestRunForTurnNextTurnSeesUpdatedPolicy(t *testing.T) {
	security := policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass)
	registry := tool.NewRegistry(nil, nil)
	if err := registry.Register(&turnWriteTool{}); err != nil {
		t.Fatal(err)
	}
	providerRuntime := &scriptedProvider{streams: []provider.Stream{
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventToolCallDelta, ToolCall: &provider.ToolCallFragment{
				ID: "w1", Name: "write", Arguments: `{"path":"a","value":"x"}`,
			}},
			{Type: provider.EventMessageStop},
		}},
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventTextDelta, Text: "first"},
			{Type: provider.EventMessageStop},
		}},
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventToolCallDelta, ToolCall: &provider.ToolCallFragment{
				ID: "w2", Name: "write", Arguments: `{"path":"b","value":"y"}`,
			}},
			{Type: provider.EventMessageStop},
		}},
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventTextDelta, Text: "second"},
			{Type: provider.EventMessageStop},
		}},
	}}
	engine, err := newTestEngine(Options{ProviderConfig: ProviderConfig{Provider: providerRuntime, Route: testRoute(t),
		MaxOutputTokens: 128}, ToolConfig: ToolConfig{Tools: registry,

		Authorize: func(provider.ToolCall) bool { return true }}, SecurityConfig: SecurityConfig{Security: security, Workspace: t.TempDir()},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.RunForTurn(t.Context(), "t1", "first", nil); err != nil {
		t.Fatal(err)
	}
	security.Permission = policy.PermissionSuggest
	var approvals int
	_, err = engine.RunForTurn(t.Context(), "t2", "second", func(event Event) error {
		if event.State == AwaitingApproval && event.Approval != nil {
			approvals++
			_ = mustControl(t, engine).ResolveApproval(toolguard.ApprovalDecision{
				RequestID: event.Approval.RequestID, Approved: true,
				Scope: policy.ApprovalOnce, ExpiresAt: time.Now().Add(time.Minute),
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if approvals != 1 {
		t.Fatalf("second turn approvals = %d, want 1 under suggest", approvals)
	}
}

type turnWriteTool struct{}

func (turnWriteTool) Descriptor() tool.Descriptor {
	return tool.Descriptor{
		Name: "write", Description: "test write", Visibility: tool.VisibleModel,
		Capability: tool.CapabilityWrite, AccessMode: tool.AccessWrite,
		ParallelPolicy: tool.ParallelConcurrent, SandboxRequirement: tool.SandboxNone,
		Availability: tool.AvailabilityAvailable,
		ResourceResolver: tool.ResourceResolver{Templates: []tool.ResourceTemplate{{
			Kind: "file", Field: "path", Access: tool.AccessWrite,
		}}},
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":  map[string]any{"type": "string"},
				"value": map[string]any{"type": "string"},
			},
			"required": []string{"path", "value"}, "additionalProperties": false,
		},
	}
}

func (turnWriteTool) Execute(_ context.Context, raw json.RawMessage) (tool.Result, error) {
	return tool.Result{Content: string(raw)}, nil
}

type turnContextBackend struct{}

func (turnContextBackend) Capability() sandbox.Capability {
	return sandbox.Capability{
		Platform: "test", Backend: "test", Available: true,
		Effective: controlmatrix.Matrix{
			FilesystemRead:  controlmatrix.FilesystemReadDeclaredRoots,
			FilesystemWrite: controlmatrix.FilesystemWriteExactPaths,
			Network:         controlmatrix.NetworkDenied,
			ProcessTree:     controlmatrix.ProcessTreeGroupKill,
			CrossProcess:    controlmatrix.CrossProcessUnrestricted,
			Syscall:         controlmatrix.SyscallDenyDangerous,
			IPC:             controlmatrix.IPCUnrestricted,
			PathIdentity:    controlmatrix.PathIdentityDescriptorRelative,
			ArtifactOrigin:  controlmatrix.ArtifactOriginUnverifiedPath,
			DurableRecovery: controlmatrix.DurableRecoveryMemoryOnly,
		},
	}
}

func (turnContextBackend) Prepare(_ context.Context, command sandbox.Command) (sandbox.Command, error) {
	return command, nil
}
