package subagent

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/common/tracecontext"
)

func TestDelegationPolicyExplicitAndAdaptiveTriggers(t *testing.T) {
	explicit, err := NewDelegationPolicy(DelegationExplicit)
	if err != nil {
		t.Fatal(err)
	}
	base := DelegationIntent{
		TaskName: "inspect_runtime", Role: RoleExplore,
		Objective: "inspect runtime", ExpectedOutput: "key files and findings",
	}
	base.Trigger = TriggerAdaptive
	if err := explicit.Admit(base); err == nil {
		t.Fatal("explicit policy accepted adaptive trigger")
	}
	base.Trigger = TriggerUser
	if err := explicit.Admit(base); err != nil {
		t.Fatalf("explicit user trigger: %v", err)
	}
	adaptive, err := NewDelegationPolicy(DelegationAdaptive)
	if err != nil {
		t.Fatal(err)
	}
	base.Trigger = TriggerAdaptive
	if err := adaptive.Admit(base); err != nil {
		t.Fatalf("adaptive trigger: %v", err)
	}
	disabled, err := NewDelegationPolicy(DelegationDisabled)
	if err != nil {
		t.Fatal(err)
	}
	if disabled.ModelVisible() || disabled.Instructions() != "" {
		t.Fatal("disabled policy exposed model delegation")
	}
	if err := disabled.Admit(base); err == nil {
		t.Fatal("disabled policy accepted spawn")
	}
	base.Trigger = TriggerSystem
	if err := disabled.Admit(base); err != nil {
		t.Fatalf("disabled policy rejected internal system task: %v", err)
	}
}

func TestDelegationIntentRejectsUnsafeOwnership(t *testing.T) {
	policy, err := NewDelegationPolicy(DelegationExplicit)
	if err != nil {
		t.Fatal(err)
	}
	intent := DelegationIntent{
		TaskName: "write_outside", Role: RoleImplementer,
		Objective: "write outside", ExpectedOutput: "a patch",
		OwnedPaths: []string{"../outside"}, Trigger: TriggerUser,
	}
	if err := policy.Admit(intent); err == nil || !strings.Contains(err.Error(), "workspace-relative") {
		t.Fatalf("unsafe owned path error = %v", err)
	}
}

func TestRoleCatalogAndAgentControlFreezeSpawnContract(t *testing.T) {
	manager, err := Open(Options{
		Root: t.TempDir(), Gate: &fakeGate{},
	})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := NewDelegationPolicy(DelegationExplicit)
	if err != nil {
		t.Fatal(err)
	}
	control, err := NewAgentControl(
		manager,
		DefaultRoleCatalog(),
		policy,
	)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := control.SpawnIntent(DelegationIntent{
		TaskName: "inspect_runtime", Role: RoleExplore,
		Objective: "inspect runtime", ExpectedOutput: "key files and findings",
		Trigger: TriggerDeveloper,
	})
	if err != nil {
		t.Fatal(err)
	}
	if agent.Role != RoleExplore || agent.Stance != StanceReadOnly ||
		agent.TaskName != "inspect_runtime" ||
		agent.DelegationTrigger != TriggerDeveloper ||
		!strings.Contains(agent.RoleInstructions, "do not modify") {
		t.Fatalf("spawned agent = %+v", agent)
	}
}

func TestReviewRoleAllowsReadOnlyProcesses(t *testing.T) {
	role, err := DefaultRoleCatalog().Resolve(RoleReview)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(role.AllowedTools, "process.read_only") {
		t.Fatalf("review allowed tools = %v, want process.read_only", role.AllowedTools)
	}
}

func TestAgentControlPropagatesChildTraceIntoTurn(t *testing.T) {
	runtime := &recordingRuntime{}
	manager, err := Open(Options{
		Root: t.TempDir(), Gate: &fakeGate{}, Runtime: runtime,
	})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := NewDelegationPolicy(DelegationExplicit)
	if err != nil {
		t.Fatal(err)
	}
	control, err := NewAgentControl(
		manager,
		DefaultRoleCatalog(),
		policy,
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := tracecontext.NewRoot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	parent, _ := tracecontext.Current(ctx)
	agent, err := control.SpawnIntentContext(ctx, DelegationIntent{
		TaskName: "trace_runtime", Role: RoleExplore,
		Objective: "trace runtime", ExpectedOutput: "trace evidence",
		Trigger: TriggerDeveloper,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Takeover(
		context.Background(),
		agent.ID,
		"trace",
	); err != nil {
		t.Fatal(err)
	}
	child := runtime.traceLink()
	if child.TraceID != parent.TraceID ||
		child.SpanID == "" ||
		child.SpanID == parent.SpanID {
		t.Fatalf("parent=%+v child=%+v", parent, child)
	}
}
