package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	promptcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/prompt"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	"github.com/fwtllh-png/QCode/internal/security/policy"
)

func TestEffectiveProfilePermissionPreservesHostReadOnlyCeiling(t *testing.T) {
	for _, requested := range []policy.Permission{
		policy.PermissionSuggest,
		policy.PermissionAuto,
		policy.PermissionBypass,
	} {
		if got := effectiveProfilePermission(true, requested); got != policy.PermissionNever {
			t.Fatalf("read-only permission for %s = %s", requested, got)
		}
	}
	if got := effectiveProfilePermission(
		false,
		policy.PermissionBypass,
	); got != policy.PermissionBypass {
		t.Fatalf("trusted permission = %s", got)
	}
}

func TestProfilePermissionCeilingDistinguishesHostFromSessionNever(t *testing.T) {
	sessionNever := Options{SecurityConfig: SecurityConfig{Security: policy.DefaultRuntime(policy.ModeAct, policy.PermissionNever),
		ProfilePermissionCeiling: policy.PermissionSuggest},
	}
	if profileReadOnlyFromOptions(sessionNever) {
		t.Fatal("session-selected never became a Host read-only ceiling")
	}
	hostNever := sessionNever
	hostNever.ProfilePermissionCeiling = policy.PermissionNever
	if !profileReadOnlyFromOptions(hostNever) {
		t.Fatal("Host read-only ceiling was not retained")
	}
}

func TestProfilePermissionCeilingClampsEveryPosture(t *testing.T) {
	for _, requested := range []policy.Permission{
		policy.PermissionNever,
		policy.PermissionSuggest,
		policy.PermissionAuto,
		policy.PermissionBypass,
	} {
		got := effectiveProfilePermissionWithCeiling(
			false, requested, policy.PermissionSuggest,
		)
		want := requested
		if requested == policy.PermissionAuto || requested == policy.PermissionBypass {
			want = policy.PermissionSuggest
		}
		if got != want {
			t.Fatalf("requested %q under suggest ceiling = %q, want %q", requested, got, want)
		}
	}
}

func TestSessionProfileUsesAutomaticPlanningPolicy(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	route := engine.options.Routes.Act()
	profile := protocol.SessionProfile{
		Version: protocol.SessionProfileVersion, Revision: 2,
		Mode: "act", PlanningPolicy: "required",
		Provider: route.ProviderID(), Model: route.Model().ID,
		ApprovalPosture: "suggest", ExecutionTarget: "local",
		MaxSteps: 8, PromptCacheRevision: 2,
	}
	if err := engine.ApplySessionProfile(profile); err != nil {
		t.Fatal(err)
	}
	planning := engine.options.Security.PlanningSnapshot()
	if planning.Planning != string(policy.PlanningAdaptive) {
		t.Fatalf("effective planning policy = %q", planning.Planning)
	}
}

func TestSessionProfileModeProjectsThroughWorldState(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	route := engine.options.Routes.Act()
	profile := protocol.SessionProfile{
		Version: protocol.SessionProfileVersion, Revision: 2,
		Mode: "plan", Provider: route.ProviderID(), Model: route.Model().ID,
		ApprovalPosture: "suggest", ExecutionTarget: "local",
		MaxSteps: 8, PromptCacheRevision: 2,
	}
	if err := engine.ApplySessionProfile(profile); err != nil {
		t.Fatal(err)
	}
	spec, err := SnapshotTurnSpec(
		engine.options,
		TurnIdentity{
			SessionID: "session", TurnID: "turn",
			ProfileRevision: profile.Revision,
		},
		TurnRequest{},
	)
	if err != nil {
		t.Fatal(err)
	}
	sections, receipts := engine.frozenWorldSections(spec, 1)
	var mode string
	for _, section := range sections {
		if section.ID == promptcontext.PartitionMode &&
			section.Message != nil {
			mode = section.Message.Text()
		}
	}
	if !strings.Contains(mode, "Mode: plan") ||
		strings.Contains(mode, "Mode: act") {
		t.Fatalf("projected mode = %q", mode)
	}
	found := false
	for _, receipt := range receipts {
		found = found || receipt.Kind == promptcontext.PartitionMode
	}
	if !found {
		t.Fatalf("world receipts = %+v", receipts)
	}
}

func TestSessionProfileSelectsAvailableModelBetweenTurns(t *testing.T) {
	resolver, err := model.NewResolver(testCatalog(t))
	if err != nil {
		t.Fatal(err)
	}
	chat, err := resolver.Resolve(model.RouteRequest{
		ProviderID: "deepseek",
		ModelID:    "deepseek-chat",
	})
	if err != nil {
		t.Fatal(err)
	}
	reasoner, err := resolver.Resolve(model.RouteRequest{
		ProviderID: "deepseek",
		ModelID:    "deepseek-reasoner",
	})
	if err != nil {
		t.Fatal(err)
	}
	routes, err := model.NewRouteSet(chat, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := newTestEngine(Options{
		ProviderConfig: ProviderConfig{
			Provider: &scriptedProvider{},
			Route:    chat,
			Routes:   routes,
			SelectableRoutes: map[string]model.ReadyRoute{
				model.RouteKey(chat.ProviderID(), chat.Model().ID):         chat,
				model.RouteKey(reasoner.ProviderID(), reasoner.Model().ID): reasoner,
			},
			MaxOutputTokens: 128,
		},
		ToolConfig: ToolConfig{Tools: tool.NewRegistry(nil, nil)},
		SecurityConfig: SecurityConfig{
			Security: policy.DefaultRuntime(
				policy.ModeAct,
				policy.PermissionSuggest,
			),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	profile := protocol.SessionProfile{
		Version: protocol.SessionProfileVersion, Revision: 2,
		Mode: "act", Provider: "deepseek", Model: "deepseek-reasoner",
		ReasoningEffort: "high", ApprovalPosture: "suggest",
		ExecutionTarget: "local", MaxSteps: 8, PromptCacheRevision: 2,
	}
	if err := engine.ApplySessionProfile(profile); err != nil {
		t.Fatal(err)
	}
	spec, err := SnapshotTurnSpec(
		engine.options,
		TurnIdentity{
			SessionID: "session", TurnID: "turn",
			ProfileRevision: profile.Revision,
		},
		TurnRequest{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Route.Model().ID != "deepseek-reasoner" ||
		spec.Profile.ReasoningEffort != "high" {
		t.Fatalf("turn spec route/profile = %+v / %+v", spec.Route.Model(), spec.Profile)
	}
}

func TestSessionProfileModelChangeRotatesTokenWindowAndPreparedCompaction(t *testing.T) {
	resolver, err := model.NewResolver(testCatalog(t))
	if err != nil {
		t.Fatal(err)
	}
	chat, err := resolver.Resolve(model.RouteRequest{
		ProviderID: "deepseek",
		ModelID:    "deepseek-chat",
	})
	if err != nil {
		t.Fatal(err)
	}
	reasoner, err := resolver.Resolve(model.RouteRequest{
		ProviderID: "deepseek",
		ModelID:    "deepseek-reasoner",
	})
	if err != nil {
		t.Fatal(err)
	}
	routes, err := model.NewRouteSet(chat, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := newTestEngine(Options{
		ProviderConfig: ProviderConfig{
			Provider: &scriptedProvider{},
			Route:    chat,
			Routes:   routes,
			SelectableRoutes: map[string]model.ReadyRoute{
				model.RouteKey(chat.ProviderID(), chat.Model().ID):         chat,
				model.RouteKey(reasoner.ProviderID(), reasoner.Model().ID): reasoner,
			},
			MaxOutputTokens: 128,
		},
		ToolConfig: ToolConfig{Tools: tool.NewRegistry(nil, nil)},
	})
	if err != nil {
		t.Fatal(err)
	}
	before := engine.context.Window()
	observed := protocol.SampleContextData{
		ContextDigest:   "sha256:old-model",
		EstimatedTokens: 100,
	}
	before.Observe(observed, 175, 0)
	engine.context.SetWindow(before)
	engine.context.SetCompaction(agentcontext.Compaction{
		State: &agentcontext.CompactionState{
			ID:    "old-model-candidate",
			Phase: "prepared",
		},
	})

	profile := protocol.SessionProfile{
		Version:             protocol.SessionProfileVersion,
		Revision:            2,
		Mode:                "act",
		Provider:            "deepseek",
		Model:               "deepseek-reasoner",
		ReasoningEffort:     "high",
		ApprovalPosture:     "suggest",
		ExecutionTarget:     "local",
		MaxSteps:            8,
		PromptCacheRevision: 2,
	}
	if err := engine.ApplySessionProfile(profile); err != nil {
		t.Fatal(err)
	}
	after := engine.context.Window()
	if after.ID == before.ID ||
		after.Number != before.Number+1 ||
		after.PrefillObserved ||
		after.LastProviderInputTokens != 0 {
		t.Fatalf("rotated window=%+v, old=%+v", after, before)
	}
	if state := engine.context.Compaction().State; state != nil {
		t.Fatalf("old-model compaction survived route change: %+v", state)
	}
}

func TestSessionProfileRejectsNewModelWithoutMetadata(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	base := engine.options.Routes.Act()
	profile := protocol.SessionProfile{
		Version: protocol.SessionProfileVersion, Revision: 2,
		Mode: "act", Provider: base.ProviderID(), Model: "model-released-today",
		ApprovalPosture: "suggest", ExecutionTarget: "local",
		MaxSteps: 8, PromptCacheRevision: 2,
	}
	if err := engine.ApplySessionProfile(profile); err == nil {
		t.Fatal("profile accepted a model without explicit metadata")
	}
}

func TestSessionProfileToolAllowlistRejectsUnadvertisedExecution(t *testing.T) {
	executor := &profileToolExecutor{}
	registry := tool.NewRegistry(nil, nil)
	if err := registry.Register(executor); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, &scriptedProvider{}, registry)
	route := engine.options.Routes.Act()
	profile := protocol.SessionProfile{
		Version: protocol.SessionProfileVersion, Revision: 1,
		Mode: "act", Provider: route.ProviderID(), Model: route.Model().ID,
		EnabledToolIDs: []string{"builtin:result_get"}, ApprovalPosture: "suggest",
		ExecutionTarget: "local", MaxSteps: 8, PromptCacheRevision: 1,
	}
	if err := engine.ApplySessionProfile(profile); err != nil {
		t.Fatal(err)
	}
	if definitions := testToolDefinitions(t, engine); len(definitions) != 0 {
		t.Fatalf("definitions = %+v, want only hidden retrieval helper", definitions)
	}
	results, err := engine.runTools(
		context.Background(),
		"turn-profile-tools",
		[]provider.ToolCall{{
			ID: "call-disabled", Name: "profile_write", Arguments: `{}`,
		}},
		map[string]tool.Result{},
		func(State, Event) error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].IsError ||
		results[0].Metadata["error_category"] != "tool_disabled" {
		t.Fatalf("results = %+v", results)
	}
	if executor.calls != 0 {
		t.Fatalf("disabled executor ran %d times", executor.calls)
	}
}

func TestSessionProfileToolAllowlistDoesNotBypassGuard(t *testing.T) {
	executor := &profileToolExecutor{}
	registry := tool.NewRegistry(nil, nil)
	if err := registry.Register(executor); err != nil {
		t.Fatal(err)
	}
	engine, err := newTestEngine(Options{ProviderConfig: ProviderConfig{Provider: &scriptedProvider{}, Route: testRoute(t),
		MaxOutputTokens: 128}, ToolConfig: ToolConfig{Tools: registry,

		Authorize: func(provider.ToolCall) bool { return false }},
	})
	if err != nil {
		t.Fatal(err)
	}
	route := engine.options.Routes.Act()
	profile := protocol.SessionProfile{
		Version: protocol.SessionProfileVersion, Revision: 1,
		Mode: "act", Provider: route.ProviderID(), Model: route.Model().ID,
		EnabledToolIDs: []string{"builtin:profile_write"}, ApprovalPosture: "suggest",
		ExecutionTarget: "local", MaxSteps: 8, PromptCacheRevision: 1,
	}
	if err := engine.ApplySessionProfile(profile); err != nil {
		t.Fatal(err)
	}
	results, err := engine.runTools(
		context.Background(),
		"turn-profile-guard",
		[]provider.ToolCall{{
			ID: "call-guarded", Name: "profile_write", Arguments: `{}`,
		}},
		map[string]tool.Result{},
		func(State, Event) error { return nil },
	)
	if err == nil && (len(results) != 1 || !results[0].IsError) {
		t.Fatalf("guard accepted selected write tool: %+v", results)
	}
	if executor.calls != 0 {
		t.Fatalf("guarded executor ran %d times", executor.calls)
	}
}

func TestSessionProfileToolAllowlistBindsSourceAcrossRevocation(t *testing.T) {
	original := &profileToolExecutor{}
	registry := tool.NewRegistry(nil, nil)
	if err := registry.Register(original); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, &scriptedProvider{}, registry)
	route := engine.options.Routes.Act()
	profile := protocol.SessionProfile{
		Version: protocol.SessionProfileVersion, Revision: 1,
		Mode: "act", Provider: route.ProviderID(), Model: route.Model().ID,
		EnabledToolIDs:  []string{"builtin:profile_write"},
		ApprovalPosture: "suggest", ExecutionTarget: "local",
		MaxSteps: 8, PromptCacheRevision: 1,
	}
	if err := engine.ApplySessionProfile(profile); err != nil {
		t.Fatal(err)
	}
	snapshot, err := registry.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := snapshot.Lookup("profile_write")
	if !ok {
		t.Fatal("profile_write is missing")
	}
	if _, err := registry.Revoke(
		entry.Source,
		"profile_write",
		registry.Generation(),
	); err != nil {
		t.Fatal(err)
	}
	replacement := &profileToolExecutor{}
	if _, err := registry.Reconcile(
		"dynamic:replacement",
		registry.Generation(),
		[]tool.Registration{trustedExternalRegistration(replacement)},
	); err != nil {
		t.Fatal(err)
	}
	if definitions := testToolDefinitions(t, engine); len(definitions) != 0 {
		t.Fatalf("same-name replacement inherited allowlist: %+v", definitions)
	}
	results, err := engine.runTools(
		context.Background(),
		"turn-profile-source",
		[]provider.ToolCall{{
			ID: "call-replacement", Name: "profile_write", Arguments: `{}`,
		}},
		map[string]tool.Result{},
		func(State, Event) error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].IsError ||
		results[0].Metadata["error_category"] != "tool_disabled" {
		t.Fatalf("replacement result = %+v", results)
	}
	if replacement.calls != 0 {
		t.Fatalf("same-name replacement ran %d times", replacement.calls)
	}
}

type profileToolExecutor struct{ calls int }

func (e *profileToolExecutor) Descriptor() tool.Descriptor {
	return tool.Descriptor{
		Name: "profile_write", Description: "Profile allowlist fixture",
		InputSchema: map[string]any{
			"type": "object", "additionalProperties": false,
		},
		Visibility: tool.VisibleModel, Capability: tool.CapabilityWrite,
		AccessMode: tool.AccessWrite, ParallelPolicy: tool.ParallelSerial,
		SandboxRequirement: tool.SandboxNone,
		Availability:       tool.AvailabilityAvailable,
	}
}

func (e *profileToolExecutor) Execute(
	context.Context,
	json.RawMessage,
) (tool.Result, error) {
	e.calls++
	return tool.Result{Content: "executed"}, nil
}
