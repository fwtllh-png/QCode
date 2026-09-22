package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	agenttool "github.com/fwtllh-png/QCode/internal/adapter/tool/agent"
	toolguard "github.com/fwtllh-png/QCode/internal/adapter/tool/guard"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/handle"
	"github.com/fwtllh-png/QCode/internal/orchestration/subagent"
	"github.com/fwtllh-png/QCode/internal/security/policy"
	"github.com/fwtllh-png/QCode/internal/testutil/tooltest"
)

type recordingGate struct {
	calls int
}

func (g *recordingGate) Execute(
	_ context.Context, _, name string, _ json.RawMessage,
) (tool.Result, error) {
	g.calls++
	return tool.Result{Content: "gated:" + name}, nil
}

type dualRuntime struct {
	turns    []string
	onCancel func(string, string) error
}

func (r *dualRuntime) StartTurn(_ context.Context, agentID, prompt string) (string, error) {
	turn := "child-turn:" + agentID + ":" + prompt
	r.turns = append(r.turns, turn)
	return turn, nil
}

func (r *dualRuntime) CancelTurn(_ context.Context, agentID, turnID string) error {
	if r.onCancel != nil {
		return r.onCancel(agentID, turnID)
	}
	return nil
}

type staticContextSource struct {
	snapshot subagent.ParentContextSnapshot
}

func (s staticContextSource) Snapshot(
	context.Context,
	subagent.ContextSourceRef,
) (subagent.ParentContextSnapshot, error) {
	return s.snapshot, nil
}

func TestAgentSpawnReceiptAndHandleRead(t *testing.T) {
	root := t.TempDir()
	handles := handle.NewStore()
	gate := &recordingGate{}
	runtime := &dualRuntime{}
	registry := tool.NewRegistry(nil, nil)
	if err := handle.Register(registry, handles); err != nil {
		t.Fatal(err)
	}
	if err := agenttool.Register(registry, agenttool.Options{
		Handles: handles, Root: root, Gate: gate,
		Runtime: runtime, Budget: subagent.Budget{MaxDepth: 3, MaxParallel: 4}, SessionID: "session-1",
	}); err != nil {
		t.Fatal(err)
	}

	parent := execute(t, registry, "spawn_agent", spawnInput("parent_work", "parent work", "general"))
	var parentBody map[string]any
	if err := json.Unmarshal([]byte(parent.Content), &parentBody); err != nil {
		t.Fatal(err)
	}
	parentID, _ := parentBody["agent_id"].(string)
	threadID, _ := parentBody["thread_id"].(string)
	if parentID == "" || !strings.HasPrefix(threadID, "thread-") {
		t.Fatalf("parent = %+v", parentBody)
	}
	runReceipt, _ := parentBody["run_receipt"].(map[string]any)
	usage, _ := runReceipt["usage"].(map[string]any)
	verification, _ := runReceipt["verification"].(map[string]any)
	// Spawn only submits the child turn; usage and verification are pending until
	// the child settles with a real result.
	if usage["status"] != "pending" || verification["status"] != "pending" {
		t.Fatalf("run_receipt = %+v", runReceipt)
	}
	if runReceipt["task_name"] != "parent_work" ||
		runReceipt["expected_output"] != "Return a concise result with evidence." ||
		runReceipt["trigger"] != "user" {
		t.Fatalf("delegation receipt = %+v", runReceipt)
	}
	if len(runtime.turns) != 1 {
		t.Fatalf("parent turns = %#v", runtime.turns)
	}

	childInput := spawnInput("child_work", "child work", "reviewer")
	childInput["parent_id"] = parentID
	child := execute(t, registry, "spawn_agent", childInput)
	var childBody map[string]any
	if err := json.Unmarshal([]byte(child.Content), &childBody); err != nil {
		t.Fatal(err)
	}
	if childBody["depth"] != float64(1) || childBody["parent_id"] != parentID {
		t.Fatalf("child = %+v", childBody)
	}
	if childBody["role"] != "review" || childBody["stance"] != "read_only" {
		t.Fatalf("child role/stance = %+v", childBody)
	}
	if len(runtime.turns) != 2 {
		t.Fatalf("dual turns = %#v", runtime.turns)
	}

	handleRaw, err := json.Marshal(childBody["transcript_handle"])
	if err != nil {
		t.Fatal(err)
	}
	read := execute(t, registry, "handle_read", map[string]any{
		"handle": json.RawMessage(handleRaw), "mode": "head", "max_bytes": 512,
	})
	if !strings.Contains(read.Content, "agent_id=") || !strings.Contains(read.Content, "child work") {
		t.Fatalf("handle_read = %+v", read)
	}

	receipt, _ := childBody["receipt"].(map[string]any)
	if receipt["sequence"] != float64(1) || receipt["to"] != parentID {
		t.Fatalf("receipt = %+v", receipt)
	}
}

func TestAgentToolSurfaceIsExplicitAndPolicyVisible(t *testing.T) {
	registry := tool.NewRegistry(nil, nil)
	if err := agenttool.Register(registry, agenttool.Options{
		Handles: handle.NewStore(),
		Root:    t.TempDir(), Gate: &recordingGate{}, SessionID: "session-1",
	}); err != nil {
		t.Fatal(err)
	}
	descriptors := registry.Descriptors(tool.VisibleModel)
	names := make([]string, 0, len(descriptors))
	var spawnDescriptor, waitDescriptor, closeDescriptor, integrateDescriptor tool.Descriptor
	for _, descriptor := range descriptors {
		if descriptor.Name != "result_get" {
			names = append(names, descriptor.Name)
		}
		if descriptor.Name == "spawn_agent" {
			spawnDescriptor = descriptor
		}
		if descriptor.Name == "wait_agent" {
			waitDescriptor = descriptor
		}
		if descriptor.Name == "close_agent" {
			closeDescriptor = descriptor
		}
		if descriptor.Name == "integrate_agent" {
			integrateDescriptor = descriptor
		}
	}
	want := []string{
		"close_agent", "followup_task", "integrate_agent", "interrupt_agent",
		"list_agents", "send_message", "spawn_agent", "wait_agent",
	}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("model-visible agent tools = %v, want %v", names, want)
	}
	if waitDescriptor.ParallelPolicy != tool.ParallelConcurrent {
		t.Fatalf(
			"wait_agent parallel policy = %q, want concurrent",
			waitDescriptor.ParallelPolicy,
		)
	}
	if spawnDescriptor.ParallelPolicy != tool.ParallelConcurrent ||
		closeDescriptor.ParallelPolicy != tool.ParallelConcurrent {
		t.Fatalf(
			"agent lifecycle parallel policies: spawn=%q close=%q",
			spawnDescriptor.ParallelPolicy,
			closeDescriptor.ParallelPolicy,
		)
	}
	hasGitProcess := false
	for _, template := range integrateDescriptor.ResourceResolver.Templates {
		if template.Kind == "process" && template.ID == "git" &&
			template.Access == tool.AccessRead {
			hasGitProcess = true
		}
	}
	if !hasGitProcess {
		t.Fatal("integrate_agent does not declare its internal git process")
	}
	if integrateDescriptor.SandboxRequirement != tool.SandboxStrong {
		t.Fatalf(
			"integrate_agent sandbox = %q, want strong",
			integrateDescriptor.SandboxRequirement,
		)
	}
	properties := spawnDescriptor.InputSchema["properties"].(map[string]any)
	for _, field := range []string{"context_mode", "context_turns"} {
		if _, ok := properties[field]; !ok {
			t.Fatalf("spawn_agent schema is missing %q", field)
		}
	}
	for _, legacy := range []string{"fork_context", "parent_context"} {
		if _, ok := properties[legacy]; ok {
			t.Fatalf("spawn_agent schema still exposes legacy field %q", legacy)
		}
	}

	disabled := tool.NewRegistry(nil, nil)
	if err := agenttool.Register(disabled, agenttool.Options{
		Handles: handle.NewStore(),
		Root:    t.TempDir(), Gate: &recordingGate{},
		Delegation: subagent.DelegationDisabled, SessionID: "session-2",
	}); err != nil {
		t.Fatal(err)
	}
	if got := disabled.Descriptors(tool.VisibleModel); len(got) != 1 || got[0].Name != "result_get" {
		t.Fatalf("disabled model-visible tools = %v", got)
	}
	if got := disabled.Descriptors(tool.VisibleInternal); len(got) != len(want) {
		t.Fatalf("disabled internal tools = %d, want %d", len(got), len(want))
	}
}

func TestSendMessageQueuesWithoutStartingTurn(t *testing.T) {
	runtime := &dualRuntime{}
	registry := tool.NewRegistry(nil, nil)
	if err := agenttool.Register(registry, agenttool.Options{
		Handles: handle.NewStore(),
		Root:    t.TempDir(), Gate: &recordingGate{}, Runtime: runtime, SessionID: "session-1",
	}); err != nil {
		t.Fatal(err)
	}
	spawned := execute(
		t, registry, "spawn_agent",
		spawnInput("message_target", "wait for context", "explore"),
	)
	var body map[string]any
	if err := json.Unmarshal([]byte(spawned.Content), &body); err != nil {
		t.Fatal(err)
	}
	agentID, _ := body["agent_id"].(string)
	before := len(runtime.turns)
	queued := execute(t, registry, "send_message", map[string]any{
		"agent_id": agentID, "message": "focus on recovery",
	})
	if len(runtime.turns) != before {
		t.Fatalf("send_message started a turn: %#v", runtime.turns)
	}
	var queuedBody map[string]any
	if err := json.Unmarshal([]byte(queued.Content), &queuedBody); err != nil {
		t.Fatal(err)
	}
	if queuedBody["queued"] != true || queuedBody["agent_id"] != agentID {
		t.Fatalf("queued message = %+v", queuedBody)
	}
}

func TestAgentTaskCapsuleUsesRuntimeParentSnapshot(t *testing.T) {
	root := t.TempDir()
	handles := handle.NewStore()
	runtime := &dualRuntime{}
	registry := tool.NewRegistry(nil, nil)
	manager, err := subagent.Open(subagent.Options{
		Root: root, Gate: &recordingGate{}, Runtime: runtime,
	})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := subagent.NewDelegationPolicy(subagent.DelegationExplicit)
	if err != nil {
		t.Fatal(err)
	}
	control, err := subagent.NewAgentControl(
		manager, subagent.DefaultRoleCatalog(), policy,
	)
	if err != nil {
		t.Fatal(err)
	}
	control.BindContextSource(staticContextSource{
		snapshot: subagent.ParentContextSnapshot{
			SourceThread: "thread-parent", SourceTurn: "turn-parent",
			ParentGoal:     "PARENT_MARKER_ALPHA decisions=keep-plan",
			UserRequest:    "review the runtime api_key=do-not-leak",
			WorkspaceRules: []string{"follow AGENTS.md"},
		},
	})
	if err := handle.Register(registry, handles); err != nil {
		t.Fatal(err)
	}
	if err := agenttool.Register(registry, agenttool.Options{
		Control: control, Handles: handles, SessionID: "session-1",
	}); err != nil {
		t.Fatal(err)
	}
	forkInput := spawnInput("continue_review", "continue review", "explore")
	forked, err := tooltest.Execute(
		tool.WithInvocationIdentity(t.Context(), tool.InvocationIdentity{
			ThreadID: "thread-parent", TurnID: "turn-parent",
		}), registry,

		tool.Call{
			Name: "spawn_agent", Arguments: mustJSON(forkInput),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(forked.Content), &body); err != nil {
		t.Fatal(err)
	}
	if body["context_mode"] != string(subagent.ContextTaskCapsule) {
		t.Fatalf("context mode = %+v", body)
	}
	handleRaw, err := json.Marshal(body["transcript_handle"])
	if err != nil {
		t.Fatal(err)
	}
	read := execute(t, registry, "handle_read", map[string]any{
		"handle": json.RawMessage(handleRaw), "mode": "head", "max_bytes": 2048,
	})
	if !strings.Contains(read.Content, "PARENT_MARKER_ALPHA") ||
		!strings.Contains(read.Content, "context_mode=task_capsule") ||
		strings.Contains(read.Content, "do-not-leak") {
		t.Fatalf("fork transcript = %s", read.Content)
	}
	if len(runtime.turns) != 1 ||
		!strings.Contains(runtime.turns[0], "PARENT_MARKER_ALPHA") ||
		strings.Contains(runtime.turns[0], "do-not-leak") {
		t.Fatalf("runtime turns = %#v", runtime.turns)
	}

	freshInput := spawnInput("fresh_explore", "fresh explore", "explore")
	freshInput["context_mode"] = "fresh"
	fresh := execute(t, registry, "spawn_agent", freshInput)
	var freshBody map[string]any
	_ = json.Unmarshal([]byte(fresh.Content), &freshBody)
	freshHandle, _ := json.Marshal(freshBody["transcript_handle"])
	freshRead := execute(t, registry, "handle_read", map[string]any{
		"handle": json.RawMessage(freshHandle), "mode": "head", "max_bytes": 2048,
	})
	if strings.Contains(freshRead.Content, "PARENT_MARKER_ALPHA") {
		t.Fatalf("fresh child should not inherit: %s", freshRead.Content)
	}
}

func TestAgentUnsupportedRoleFailClosed(t *testing.T) {
	registry := tool.NewRegistry(nil, nil)
	if err := agenttool.Register(registry, agenttool.Options{
		Handles: handle.NewStore(), Root: t.TempDir(), Gate: &recordingGate{}, SessionID: "s",
	}); err != nil {
		t.Fatal(err)
	}
	_, err := tooltest.Execute(t.Context(), registry, tool.Call{
		Name: "spawn_agent", Arguments: mustJSON(spawnInput("bad_role", "x", "nope")),
	})
	if err == nil {
		t.Fatal("expected unsupported role rejection")
	}
	if !strings.Contains(err.Error(), "role") && !strings.Contains(err.Error(), "jsonschema") {
		t.Fatalf("err = %v", err)
	}
}

func TestAgentDepthAndConcurrencyFailClosed(t *testing.T) {
	root := t.TempDir()
	handles := handle.NewStore()
	registry := tool.NewRegistry(nil, nil)
	if err := agenttool.Register(registry, agenttool.Options{
		Handles: handles, Root: root, Gate: &recordingGate{}, Budget: subagent.Budget{MaxDepth: 1, MaxParallel: 3}, SessionID: "session-1",
	}); err != nil {
		t.Fatal(err)
	}
	first := execute(t, registry, "spawn_agent", spawnInput("one", "one", "general"))
	var body map[string]any
	_ = json.Unmarshal([]byte(first.Content), &body)
	parentID, _ := body["agent_id"].(string)
	childInput := spawnInput("child", "child", "general")
	childInput["parent_id"] = parentID
	child := execute(t, registry, "spawn_agent", childInput)
	var childBody map[string]any
	_ = json.Unmarshal([]byte(child.Content), &childBody)
	childID, _ := childBody["agent_id"].(string)
	deepInput := spawnInput("too_deep", "too-deep", "general")
	deepInput["parent_id"] = childID
	_, err := tooltest.Execute(t.Context(), registry, tool.Call{
		Name: "spawn_agent", Arguments: mustJSON(deepInput),
	})
	if err == nil {
		t.Fatal("expected depth fail-closed")
	}

	registry2 := tool.NewRegistry(nil, nil)
	handles2 := handle.NewStore()
	if err := agenttool.Register(registry2, agenttool.Options{
		Handles: handles2, Root: t.TempDir(), Gate: &recordingGate{}, Budget: subagent.Budget{MaxDepth: 2, MaxParallel: 1}, SessionID: "session-1",
	}); err != nil {
		t.Fatal(err)
	}
	_ = execute(t, registry2, "spawn_agent", spawnInput("hold", "hold", "general"))
	_, err = tooltest.Execute(t.Context(), registry2, tool.Call{
		Name: "spawn_agent", Arguments: mustJSON(spawnInput("overflow", "overflow", "general")),
	})
	if err == nil {
		t.Fatal("expected concurrency fail-closed")
	}
}

func TestNestedAgentScopeBindsCallerAndRejectsSiblingControl(t *testing.T) {
	root := t.TempDir()
	manager, err := subagent.Open(subagent.Options{
		Root: root, Gate: &recordingGate{}, Runtime: &dualRuntime{},
		Worktrees: fixedWorktrees{
			path: filepath.Join(root, "parent-worktree"),
		}, Budget: subagent.Budget{
			MaxDepth: 3, MaxParallel: 4, MaxResident: 4, MaxTotal: 4,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	delegation, err := subagent.NewDelegationPolicy(subagent.DelegationExplicit)
	if err != nil {
		t.Fatal(err)
	}
	control, err := subagent.NewAgentControl(
		manager, subagent.DefaultRoleCatalog(), delegation,
	)
	if err != nil {
		t.Fatal(err)
	}
	control.BindContextSource(staticContextSource{
		snapshot: subagent.ParentContextSnapshot{
			SourceThread: "thread-parent", SourceTurn: "turn-parent",
			ParentGoal: "nested control test",
		},
	})
	registry := tool.NewRegistry(nil, nil)
	if err := agenttool.Register(registry, agenttool.Options{
		Control: control, Handles: handle.NewStore(), SessionID: "session-1",
	}); err != nil {
		t.Fatal(err)
	}
	parent := execute(t, registry, "spawn_agent", spawnInput(
		"parent", "parent", "general",
	))
	sibling := execute(t, registry, "spawn_agent", spawnInput(
		"sibling", "sibling", "general",
	))
	var parentBody, siblingBody map[string]any
	if err := json.Unmarshal([]byte(parent.Content), &parentBody); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(sibling.Content), &siblingBody); err != nil {
		t.Fatal(err)
	}
	parentID, _ := parentBody["agent_id"].(string)
	parentThread, _ := parentBody["thread_id"].(string)
	siblingID, _ := siblingBody["agent_id"].(string)
	nestedContext := tool.WithInvocationIdentity(t.Context(), tool.InvocationIdentity{
		ThreadID: parentThread, TurnID: "turn-parent",
	})

	forged := spawnInput("forged", "forged", "explore")
	forged["parent_id"] = siblingID
	if _, err := executeWithContext(
		nestedContext, registry, "spawn_agent", forged,
	); err == nil || !strings.Contains(err.Error(), "calling agent") {
		t.Fatalf("forged parent error = %v", err)
	}
	grandchild, err := executeWithContext(
		nestedContext, registry, "spawn_agent",
		spawnInput("grandchild", "inspect parent work", "explore"),
	)
	if err != nil {
		t.Fatal(err)
	}
	var grandchildBody map[string]any
	if err := json.Unmarshal([]byte(grandchild.Content), &grandchildBody); err != nil {
		t.Fatal(err)
	}
	grandchildID, _ := grandchildBody["agent_id"].(string)
	if grandchildBody["parent_id"] != parentID ||
		grandchildBody["depth"] != float64(1) ||
		grandchildBody["trigger"] != string(subagent.TriggerSystem) {
		t.Fatalf("grandchild = %+v", grandchildBody)
	}
	parentAgent, ok := manager.Agent(parentID)
	if !ok {
		t.Fatal("parent agent unavailable")
	}
	grandchildAgent, ok := manager.Agent(grandchildID)
	if !ok || grandchildAgent.ExecutionRoot != parentAgent.Worktree {
		t.Fatalf(
			"grandchild execution root = %q, parent worktree = %q, ok=%v",
			grandchildAgent.ExecutionRoot, parentAgent.Worktree, ok,
		)
	}
	if _, err := executeWithContext(
		nestedContext, registry, "send_message",
		map[string]any{"agent_id": siblingID, "message": "forbidden"},
	); err == nil || !strings.Contains(err.Error(), "descendant subtree") {
		t.Fatalf("sibling control error = %v", err)
	}
	if _, err := executeWithContext(
		nestedContext, registry, "send_message",
		map[string]any{"agent_id": grandchildID, "message": "allowed"},
	); err != nil {
		t.Fatalf("descendant control: %v", err)
	}
}

func TestAgentWorktreeCleanupLeavesSibling(t *testing.T) {
	root := t.TempDir()
	handles := handle.NewStore()
	gate := &recordingGate{}
	manager, err := subagent.Open(subagent.Options{
		Root: root, Gate: gate, Budget: subagent.Budget{MaxParallel: 4},
	})
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry(nil, nil)
	if err := agenttool.Register(registry, agenttool.Options{
		Manager: manager, Handles: handles, SessionID: "session-1",
	}); err != nil {
		t.Fatal(err)
	}
	a := execute(t, registry, "spawn_agent", spawnInput("a", "a", "general"))
	b := execute(t, registry, "spawn_agent", spawnInput("b", "b", "general"))
	var bodyA, bodyB map[string]any
	_ = json.Unmarshal([]byte(a.Content), &bodyA)
	_ = json.Unmarshal([]byte(b.Content), &bodyB)
	idA, _ := bodyA["agent_id"].(string)
	pathB, _ := bodyB["worktree"].(string)
	if err := manager.Close(idA); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(pathB, ".qcode-worktree")
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) == "" {
		t.Fatal("sibling worktree marker missing after cleanup")
	}
}

func TestAgentToolGateForced(t *testing.T) {
	root := t.TempDir()
	handles := handle.NewStore()
	gate := &recordingGate{}
	manager, err := subagent.Open(subagent.Options{Root: root, Gate: gate})
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry(nil, nil)
	if err := agenttool.Register(registry, agenttool.Options{
		Manager: manager, Handles: handles, SessionID: "session-1",
	}); err != nil {
		t.Fatal(err)
	}
	created := execute(t, registry, "spawn_agent", spawnInput("gate", "gate", "general"))
	var body map[string]any
	_ = json.Unmarshal([]byte(created.Content), &body)
	id, _ := body["agent_id"].(string)
	result, err := manager.ExecuteTool(t.Context(), id, "call-1", "exec_command", json.RawMessage(`{"command":"true"}`))
	if err != nil {
		t.Fatal(err)
	}
	if gate.calls != 1 || result.Content != "gated:exec_command" {
		t.Fatalf("gate calls=%d result=%+v", gate.calls, result)
	}
}

func TestAgentAskFailClosedWithoutApprovalHost(t *testing.T) {
	root := t.TempDir()
	handles := handle.NewStore()
	registry := tool.NewRegistry(nil, nil)
	if err := agenttool.Register(registry, agenttool.Options{
		Handles: handles, Root: root, Gate: &recordingGate{}, SessionID: "session-1",
	}); err != nil {
		t.Fatal(err)
	}
	guard, err := toolguard.New(toolguard.Options{
		Registry: registry, Policy: policy.DefaultRuntime(policy.ModeAct, policy.PermissionAuto), Workspace: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = guard.Execute(
		t.Context(), "call-1", "spawn_agent",
		mustJSON(spawnInput("needs_approval", "needs approval", "general")),
	)
	var decision *policy.DecisionError
	if !errors.As(err, &decision) || decision.Code != "approval_host_unavailable" {
		t.Fatalf("err = %v", err)
	}
}

func TestAgentSpawnWaitCloseHermetic(t *testing.T) {
	root := t.TempDir()
	handles := handle.NewStore()
	gate := &recordingGate{}
	manager, err := subagent.Open(subagent.Options{
		Root: root, Gate: gate, Runtime: &dualRuntime{}, Budget: subagent.Budget{MaxDepth: 3, MaxParallel: 4},
	})
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry(nil, nil)
	if err := handle.Register(registry, handles); err != nil {
		t.Fatal(err)
	}
	if err := agenttool.Register(registry, agenttool.Options{
		Manager: manager, Handles: handles, SessionID: "session-1",
	}); err != nil {
		t.Fatal(err)
	}

	spawned := execute(
		t, registry, "spawn_agent",
		spawnInput("hermetic_work", "hermetic work", "explore"),
	)
	var body map[string]any
	if err := json.Unmarshal([]byte(spawned.Content), &body); err != nil {
		t.Fatal(err)
	}
	agentID, _ := body["agent_id"].(string)
	worktree, _ := body["worktree"].(string)
	if agentID == "" {
		t.Fatalf("spawn = %+v", body)
	}

	listed := execute(t, registry, "list_agents", map[string]any{})
	var listBody map[string]any
	_ = json.Unmarshal([]byte(listed.Content), &listBody)
	if listBody["count"] != float64(1) {
		t.Fatalf("list = %+v", listBody)
	}

	waitErr := make(chan error, 1)
	waitResult := make(chan tool.Result, 1)
	go func() {
		result, err := tooltest.Execute(context.Background(), registry, tool.Call{
			Name: "wait_agent", Arguments: mustJSON(map[string]any{
				"agent_ids": []string{agentID},
			}),
		})
		waitResult <- result
		waitErr <- err
	}()
	time.Sleep(20 * time.Millisecond)
	if err := manager.Complete(agentID, "done"); err != nil {
		t.Fatal(err)
	}
	if err := <-waitErr; err != nil {
		t.Fatal(err)
	}
	waited := <-waitResult
	var waitBody map[string]any
	if err := json.Unmarshal([]byte(waited.Content), &waitBody); err != nil {
		t.Fatal(err)
	}
	if waitBody["timed_out"] != false {
		t.Fatalf("wait = %+v", waitBody)
	}
	agents, _ := waitBody["agents"].([]any)
	if len(agents) != 1 {
		t.Fatalf("wait agents = %+v", waitBody)
	}
	first, _ := agents[0].(map[string]any)
	if first["status"] != "completed" || first["summary"] == "" {
		t.Fatalf("wait agent = %+v", first)
	}
	if _, leaked := first["agent_path"]; leaked {
		t.Fatalf("wait card leaked full snapshot: %+v", first)
	}

	closed := execute(t, registry, "close_agent", map[string]any{"agent_id": agentID})
	var closeBody map[string]any
	_ = json.Unmarshal([]byte(closed.Content), &closeBody)
	if closeBody["closed"] != true || closeBody["status"] != "closed" {
		t.Fatalf("close = %+v", closeBody)
	}
	if _, err := os.Stat(filepath.Join(worktree, ".qcode-worktree")); !os.IsNotExist(err) {
		t.Fatalf("worktree should be removed after close: %v", err)
	}
	after := execute(t, registry, "list_agents", map[string]any{})
	var afterBody map[string]any
	_ = json.Unmarshal([]byte(after.Content), &afterBody)
	if afterBody["count"] != float64(0) {
		t.Fatalf("list after close = %+v", afterBody)
	}
}

func TestLegacyUnifiedAgentToolIsUnavailable(t *testing.T) {
	root := t.TempDir()
	handles := handle.NewStore()
	manager, err := subagent.Open(subagent.Options{
		Root: root, Gate: &recordingGate{}, Runtime: &dualRuntime{},
	})
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry(nil, nil)
	if err := agenttool.Register(registry, agenttool.Options{
		Manager: manager, Handles: handles, SessionID: "session-1",
	}); err != nil {
		t.Fatal(err)
	}
	_, err = tooltest.Execute(t.Context(), registry, tool.Call{
		Name: "agent", Arguments: mustJSON(map[string]any{}),
	})
	if !errors.Is(err, tool.ErrUnknownTool) {
		t.Fatalf("legacy tool error = %v, want unknown tool", err)
	}
}

func TestAgentWaitDefersSerializedChildUntilCallingTurnEnds(t *testing.T) {
	root := t.TempDir()
	manager, err := subagent.Open(subagent.Options{
		Root: root, Gate: &recordingGate{}, Runtime: &dualRuntime{},
		Worktrees: fixedWorktrees{path: root, serialized: true}, Budget: subagent.Budget{MaxDepth: 3, MaxParallel: 4},
	})
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry(nil, nil)
	if err := agenttool.Register(registry, agenttool.Options{
		Manager: manager, Handles: handle.NewStore(), SessionID: "session-1",
	}); err != nil {
		t.Fatal(err)
	}
	spawned := execute(
		t, registry, "spawn_agent",
		spawnInput("edit_host_workspace", "edit host workspace", "implementer"),
	)
	var spawnBody map[string]any
	if err := json.Unmarshal([]byte(spawned.Content), &spawnBody); err != nil {
		t.Fatal(err)
	}
	agentID, _ := spawnBody["agent_id"].(string)
	if spawnBody["serialized"] != true || agentID == "" {
		t.Fatalf("spawn = %+v", spawnBody)
	}
	started := time.Now()
	waited := execute(t, registry, "wait_agent", map[string]any{
		"agent_ids": []string{agentID}, "timeout_ms": 10_000,
	})
	if time.Since(started) > time.Second {
		t.Fatal("serialized wait_agent blocked while caller held the workspace")
	}
	var waitBody map[string]any
	if err := json.Unmarshal([]byte(waited.Content), &waitBody); err != nil {
		t.Fatal(err)
	}
	if waitBody["deferred"] != true || waitBody["timed_out"] != false {
		t.Fatalf("wait = %+v", waitBody)
	}
}

func TestAgentInterruptFollowUpViaTools(t *testing.T) {
	root := t.TempDir()
	handles := handle.NewStore()
	runtime := &dualRuntime{}
	manager, err := subagent.Open(subagent.Options{
		Root: root, Gate: &recordingGate{}, Runtime: runtime,
	})
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry(nil, nil)
	if err := agenttool.Register(registry, agenttool.Options{
		Manager: manager, Handles: handles, SessionID: "session-1",
	}); err != nil {
		t.Fatal(err)
	}
	spawned := execute(t, registry, "spawn_agent", spawnInput("run", "run", "general"))
	var body map[string]any
	_ = json.Unmarshal([]byte(spawned.Content), &body)
	agentID, _ := body["agent_id"].(string)

	interrupted := execute(t, registry, "interrupt_agent", map[string]any{"agent_id": agentID})
	var interruptBody map[string]any
	_ = json.Unmarshal([]byte(interrupted.Content), &interruptBody)
	if interruptBody["status"] != "running" || interruptBody["previous_status"] != "running" {
		t.Fatalf("interrupt = %+v", interruptBody)
	}
	if action, _ := interruptBody["next_action"].(string); !strings.Contains(action, "wait_agent") {
		t.Fatalf("interrupt omitted settlement guidance: %+v", interruptBody)
	}
	_, descriptor, _, err := registry.Resolve("followup_task")
	if err != nil || !strings.Contains(descriptor.Description, "interrupt_agent, then wait_agent, then followup_task") {
		t.Fatalf("followup order: %+v err=%v", descriptor, err)
	}
	earlyArgs, _ := json.Marshal(map[string]any{"agent_id": agentID, "prompt": "too soon"})
	if result, err := tooltest.Execute(t.Context(), registry, tool.Call{
		Name: "followup_task", Arguments: earlyArgs,
	}); err == nil && !result.IsError {
		t.Fatalf("followup accepted before settlement: %+v", result)
	}
	waiting := execute(t, registry, "wait_agent", map[string]any{
		"agent_ids": []string{agentID}, "timeout_ms": 1,
	})
	if waiting.Metadata["timed_out"] != true {
		t.Fatalf("wait must observe pending cancellation: %+v", waiting)
	}
	snap, _ := manager.Agent(agentID)
	if err := manager.Settle(subagent.Result{
		AgentID: agentID, ThreadID: snap.ThreadID, TurnID: snap.TurnID,
		Status: subagent.StatusInterrupted,
	}); err != nil {
		t.Fatal(err)
	}
	waited := execute(t, registry, "wait_agent", map[string]any{"agent_ids": []string{agentID}})
	if waited.Metadata["timed_out"] != false || !strings.Contains(waited.Content, `"status":"interrupted"`) {
		t.Fatalf("wait did not confirm settlement: %+v", waited)
	}
	follow := execute(t, registry, "followup_task", map[string]any{
		"agent_id": agentID, "prompt": "resume please",
	})
	var followBody map[string]any
	_ = json.Unmarshal([]byte(follow.Content), &followBody)
	if followBody["follow_up"] != true || followBody["status"] != "running" {
		t.Fatalf("followup = %+v", followBody)
	}
	if len(runtime.turns) != 2 {
		t.Fatalf("turns = %#v", runtime.turns)
	}
}

func TestWaitAgentReturnsCompactRetryableCard(t *testing.T) {
	manager, err := subagent.Open(subagent.Options{
		Root: t.TempDir(), Gate: &recordingGate{}, Runtime: &dualRuntime{},
	})
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry(nil, nil)
	if err := agenttool.Register(registry, agenttool.Options{
		Manager: manager, Handles: handle.NewStore(), SessionID: "session-1",
	}); err != nil {
		t.Fatal(err)
	}
	spawned := execute(
		t, registry, "spawn_agent",
		spawnInput("audit", "audit budget", "review"),
	)
	var body map[string]any
	if err := json.Unmarshal([]byte(spawned.Content), &body); err != nil {
		t.Fatal(err)
	}
	agentID, _ := body["agent_id"].(string)
	if err := manager.Settle(subagent.Result{
		AgentID: agentID, Status: subagent.StatusFailed,
		Summary:         "resource_exhausted: token budget exhausted: projected 17698, limit 15000",
		ReasonCode:      subagent.ReasonBudgetExhausted,
		Retryable:       true,
		SuggestedAction: subagent.SuggestedAction(subagent.ReasonBudgetExhausted),
	}); err != nil {
		t.Fatal(err)
	}
	waited := execute(t, registry, "wait_agent", map[string]any{
		"agent_ids": []string{agentID},
	})
	var waitBody map[string]any
	if err := json.Unmarshal([]byte(waited.Content), &waitBody); err != nil {
		t.Fatal(err)
	}
	agents, _ := waitBody["agents"].([]any)
	if len(agents) != 1 {
		t.Fatalf("wait = %+v", waitBody)
	}
	card, _ := agents[0].(map[string]any)
	if card["reason_code"] != subagent.ReasonBudgetExhausted ||
		card["retryable"] != true ||
		card["suggested_action"] == "" ||
		card["agent_path"] != nil {
		t.Fatalf("compact wait card = %+v", card)
	}
}

func TestAgentToolsRejectCrossSessionTargets(t *testing.T) {
	manager, err := subagent.Open(subagent.Options{
		Root: t.TempDir(), Gate: &recordingGate{}, Runtime: &dualRuntime{}, SessionID: "session-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := subagent.NewDelegationPolicy(subagent.DelegationExplicit)
	if err != nil {
		t.Fatal(err)
	}
	control, err := subagent.NewAgentControl(
		manager, subagent.DefaultRoleCatalog(), policy,
	)
	if err != nil {
		t.Fatal(err)
	}
	first, err := control.SpawnIntent(subagent.DelegationIntent{
		SessionID: "session-a", TaskName: "first", Role: subagent.RoleExplore,
		Objective: "inspect", ExpectedOutput: "report", Trigger: subagent.TriggerUser,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := control.SpawnIntent(subagent.DelegationIntent{
		SessionID: "session-b", TaskName: "second", Role: subagent.RoleExplore,
		Objective: "inspect", ExpectedOutput: "report", Trigger: subagent.TriggerUser,
	})
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry(nil, nil)
	if err := agenttool.Register(registry, agenttool.Options{
		Control: control, Handles: handle.NewStore(), SessionID: "session-a",
	}); err != nil {
		t.Fatal(err)
	}
	sessionB := tool.WithInvocationIdentity(t.Context(), tool.InvocationIdentity{
		SessionID: "session-b", ThreadID: "thread-b", TurnID: "turn-b",
	})
	if _, err := executeWithContext(
		sessionB, registry, "close_agent",
		map[string]any{"agent_id": first.ID},
	); err == nil || !strings.Contains(err.Error(), "another session") {
		t.Fatalf("cross-session close error = %v", err)
	}
	listed, err := executeWithContext(
		sessionB, registry, "list_agents", map[string]any{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(listed.Content, first.ID) ||
		!strings.Contains(listed.Content, second.ID) {
		t.Fatalf("session-b agent list = %s", listed.Content)
	}
}

func TestSpawnPostStartFailureReleasesChildRuntime(t *testing.T) {
	runtime := &dualRuntime{}
	manager, err := subagent.Open(subagent.Options{
		Root: t.TempDir(), Gate: &recordingGate{}, Runtime: runtime, SessionID: "session-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime.onCancel = func(agentID, turnID string) error {
		return manager.Settle(subagent.Result{
			AgentID: agentID, ThreadID: subagent.ThreadIDFor(agentID),
			TurnID: turnID, Status: subagent.StatusInterrupted,
		})
	}
	control, err := subagent.NewAgentControl(
		manager,
		subagent.DefaultRoleCatalog(),
		mustDelegationPolicy(t),
	)
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("persist mailbox failed")
	graph := subagent.DurableGraph{
		Workspace: "/workspace", SessionID: "session-1",
		AppendSpawn:  func(subagent.GraphEdge) error { return nil },
		AppendStatus: func(subagent.GraphTransition) error { return nil },
		AppendMessage: func(subagent.Message) error {
			return failure
		},
		DeliverMessage: func(subagent.Message) error { return nil },
		Sessions:       func() ([]string, error) { return nil, nil },
		Children: func(string, string) ([]subagent.GraphEdge, error) {
			return nil, nil
		},
		Messages: func(string, string) ([]subagent.Message, error) {
			return nil, nil
		},
		Result: func(string, string) (subagent.Result, bool, error) {
			return subagent.Result{}, false, nil
		},
		IntegrationResult: func(string, string) (subagent.Result, bool, error) {
			return subagent.Result{}, false, nil
		},
		AppendIntegration: func(subagent.IntegrationCandidate) error { return nil },
		Integration: func(
			string, string, string,
		) (subagent.IntegrationCandidate, bool, error) {
			return subagent.IntegrationCandidate{}, false, nil
		},
		Budget:         func(string) (subagent.BudgetLedger, error) { return subagent.BudgetLedger{}, nil },
		ReconcileGraph: func() error { return nil },
	}
	if err := control.AttachGraph(graph); err != nil {
		t.Fatal(err)
	}
	released := 0
	registry := tool.NewRegistry(nil, nil)
	if err := agenttool.Register(registry, agenttool.Options{
		Control: control, Handles: handle.NewStore(),
		OnRelease: func(string) {
			released++
		}, SessionID: "session-1",
	}); err != nil {
		t.Fatal(err)
	}
	input := spawnInput("cleanup", "start then fail", "explore")
	input["context_mode"] = "fresh"
	_, err = executeWithContext(
		tool.WithInvocationIdentity(t.Context(), tool.InvocationIdentity{
			SessionID: "session-1", ThreadID: "thread-parent", TurnID: "turn-parent",
		}),
		registry,
		"spawn_agent",
		input,
	)
	if !errors.Is(err, failure) {
		t.Fatalf("spawn error = %v", err)
	}
	if released != 1 || len(runtime.turns) != 1 {
		t.Fatalf("released=%d turns=%v", released, runtime.turns)
	}
	agents := control.List(subagent.ListFilter{
		SessionID: "session-1", IncludeClosed: true,
	})
	if len(agents) != 1 || !agents[0].Closed {
		t.Fatalf("post-failure agents = %+v", agents)
	}
}

func mustDelegationPolicy(t *testing.T) subagent.DelegationPolicy {
	t.Helper()
	policy, err := subagent.NewDelegationPolicy(subagent.DelegationExplicit)
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func execute(t *testing.T, registry *tool.Registry, name string, input map[string]any) tool.Result {
	t.Helper()
	result, err := tooltest.Execute(t.Context(), registry, tool.Call{
		Name: name, Arguments: mustJSON(input),
	})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return result
}

func executeWithContext(
	ctx context.Context,
	registry *tool.Registry,
	name string,
	input map[string]any,
) (tool.Result, error) {
	return tooltest.Execute(ctx, registry, tool.Call{
		Name: name, Arguments: mustJSON(input),
	})
}

func spawnInput(taskName, objective, role string) map[string]any {
	return map[string]any{
		"task_name": taskName, "objective": objective, "role": role,
		"expected_output": "Return a concise result with evidence.",
		"trigger":         "user",
	}
}

func mustJSON(value map[string]any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
}
