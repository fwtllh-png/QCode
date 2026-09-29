package wire

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
	"github.com/fwtllh-png/QCode/internal/config"
	"github.com/fwtllh-png/QCode/internal/orchestration/subagent"
	"github.com/fwtllh-png/QCode/internal/persist/state"
	agentengine "github.com/fwtllh-png/QCode/internal/runtime/agent/engine"
	"github.com/fwtllh-png/QCode/internal/runtime/app"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	securitypolicy "github.com/fwtllh-png/QCode/internal/security/policy"
)

type authorityTestTool struct{ descriptor tool.Descriptor }

func (t authorityTestTool) Descriptor() tool.Descriptor { return t.descriptor }

func (authorityTestTool) Execute(context.Context, json.RawMessage) (tool.Result, error) {
	return tool.Result{Content: "ok"}, nil
}

// subagentFixture is absolute because the session workspace is a temp directory
// and a relative fixture path is resolved against it.
func subagentFixture(t *testing.T, name string) string {
	t.Helper()
	path, err := filepath.Abs(
		filepath.Join("..", "..", "..", "..", "testdata", "providers", name),
	)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestChildAuthorityIsParentAndRoleIntersection(t *testing.T) {
	parent := tool.NewRegistry(nil, nil)
	child := tool.NewRegistry(nil, nil)
	register := func(registry *tool.Registry, name string, capability tool.Capability) {
		t.Helper()
		access := tool.AccessRead
		if capability != tool.CapabilityRead {
			access = tool.AccessWrite
		}
		err := registry.Register(authorityTestTool{descriptor: tool.Descriptor{
			Name: name, Description: name,
			InputSchema: map[string]any{"type": "object"},
			Visibility:  tool.VisibleModel, Capability: capability,
			AccessMode: access, ParallelPolicy: tool.ParallelConcurrent,
			SandboxRequirement: tool.SandboxNone,
			Availability:       tool.AvailabilityAvailable,
		}})

		if err != nil {
			t.Fatal(err)
		}
	}
	for _, registry := range []*tool.Registry{parent, child} {
		register(registry, "file_read", tool.CapabilityRead)
		register(registry, "file_write", tool.CapabilityWrite)
		register(registry, "spawn_agent", tool.CapabilityWrite)
	}
	register(child, "child_only", tool.CapabilityRead)

	security := securitypolicy.DefaultRuntime(securitypolicy.ModeAct, securitypolicy.PermissionBypass)
	restrictChildTools(security, app.ChildSpec{
		AllowedTools: []string{"read"}, CanDelegate: false,
	}, parent, child)
	decision := func(name string, capability tool.Capability) securitypolicy.Decision {
		return security.Decide(resolvePolicyFixture(policyInvocationFixture{
			CallID: name + "-call", Tool: name,
			Arguments: json.RawMessage(`{}`), Capability: capability,
			Validated: true,
		}))
	}
	if got := decision("file_read", tool.CapabilityRead); got.Action != securitypolicy.ActionAllow {
		t.Fatalf("inherited role read = %+v", got)
	}
	for name, capability := range map[string]tool.Capability{
		"file_write":  tool.CapabilityWrite,
		"spawn_agent": tool.CapabilityWrite,
		"child_only":  tool.CapabilityRead,
	} {
		if got := decision(name, capability); got.Action != securitypolicy.ActionDeny {
			t.Fatalf("%s authority = %+v, want deny", name, got)
		}
	}
}

func TestReviewChildAllowsOnlyReadOnlyProcessEffects(t *testing.T) {
	parent := tool.NewRegistry(nil, nil)
	child := tool.NewRegistry(nil, nil)
	register := func(registry *tool.Registry, name string) {
		t.Helper()
		err := registry.Register(authorityTestTool{descriptor: tool.Descriptor{
			Name: name, Description: name,
			InputSchema: map[string]any{"type": "object"},
			Visibility:  tool.VisibleModel, Capability: tool.CapabilityProcess,
			AccessMode: tool.AccessRead, ParallelPolicy: tool.ParallelConcurrent,
			SandboxRequirement: tool.SandboxStrong,
			Availability:       tool.AvailabilityAvailable,
		}})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, registry := range []*tool.Registry{parent, child} {
		register(registry, "exec_command")
		register(registry, "process_read")
	}

	role, err := subagent.DefaultRoleCatalog().Resolve(subagent.RoleReview)
	if err != nil {
		t.Fatal(err)
	}
	spec := app.ChildSpec{
		ReadOnly: true, AllowedTools: role.AllowedTools, CanDelegate: false,
	}
	options := childEngineOptions(
		agentengine.Options{SecurityConfig: agentengine.SecurityConfig{
			Security: securitypolicy.DefaultRuntime(
				securitypolicy.ModeAct,
				securitypolicy.PermissionSuggest,
			),
		}},
		spec,
	)
	restrictChildTools(options.Security, spec, parent, child)

	inspect := resolvePolicyFixture(policyInvocationFixture{
		CallID: "inspect", Tool: "process_read",
		Arguments:  json.RawMessage(`{}`),
		Capability: tool.CapabilityProcess, Access: tool.AccessRead,
		Sandbox: tool.SandboxStrong, Validated: true,
		Resources: []tool.Resource{
			{Kind: "process", ID: "workspace", Access: tool.AccessRead},
		},
	})
	if decision := options.Security.Decide(inspect); decision.Action != securitypolicy.ActionAllow {
		t.Fatalf("read-only process decision = %+v, want allow", decision)
	}

	exec := inspect
	exec.CallID = "exec"
	exec.Tool = "exec_command"
	exec.Arguments = json.RawMessage(`{"command":"rg needle ."}`)
	decision := options.Security.Decide(exec)
	if decision.Action != securitypolicy.ActionDeny {
		t.Fatalf("exec_command decision = %+v, want deny", decision)
	}
	if options.Security.AdvertisesTool("exec_command") {
		t.Fatal("review child advertised exec_command after denying it")
	}
	if !options.Security.AdvertisesTool("process_read") {
		t.Fatal("review child hid the allowed read-only process tool")
	}
}

func TestDelegatingReadOnlyRoleRetainsOnlyAgentLifecycleWrites(t *testing.T) {
	parent := tool.NewRegistry(nil, nil)
	child := tool.NewRegistry(nil, nil)
	register := func(registry *tool.Registry, name string, capability tool.Capability) {
		t.Helper()
		err := registry.Register(authorityTestTool{descriptor: tool.Descriptor{
			Name: name, Description: name,
			InputSchema: map[string]any{"type": "object"},
			Visibility:  tool.VisibleModel, Capability: capability,
			AccessMode: tool.AccessWrite, ParallelPolicy: tool.ParallelConcurrent,
			SandboxRequirement: tool.SandboxNone,
			Availability:       tool.AvailabilityAvailable,
		}})

		if err != nil {
			t.Fatal(err)
		}
	}
	for _, registry := range []*tool.Registry{parent, child} {
		register(registry, "file_read", tool.CapabilityRead)
		register(registry, "file_write", tool.CapabilityWrite)
		register(registry, "spawn_agent", tool.CapabilityWrite)
		register(registry, "list_agents", tool.CapabilityRead)
	}
	spec := app.ChildSpec{
		ReadOnly:     true,
		AllowedTools: []string{"read", "search"}, CanDelegate: true,
	}
	options := childEngineOptions(
		agentengine.Options{SecurityConfig: agentengine.SecurityConfig{
			Security: securitypolicy.DefaultRuntime(
				securitypolicy.ModeAct,
				securitypolicy.PermissionSuggest,
			),
		}},
		spec,
	)
	security := options.Security
	restrictChildTools(security, spec, parent, child)
	decision := func(name string, capability tool.Capability) securitypolicy.Action {
		return security.Decide(resolvePolicyFixture(policyInvocationFixture{
			CallID: name, Tool: name, Arguments: json.RawMessage(`{}`),
			Capability: capability, Validated: true,
		})).Action
	}
	spawnDecision := decision("spawn_agent", tool.CapabilityWrite)
	listDecision := decision("list_agents", tool.CapabilityRead)
	if options.Security.Mode != securitypolicy.ModeAct ||
		options.Security.Permission != securitypolicy.PermissionSuggest ||
		spawnDecision != securitypolicy.ActionAsk ||
		listDecision != securitypolicy.ActionAllow {
		t.Fatalf(
			"delegating read-only authority: mode=%s permission=%s spawn=%s list=%s",
			options.Security.Mode, options.Security.Permission,
			spawnDecision, listDecision,
		)
	}
	if decision("file_read", tool.CapabilityRead) != securitypolicy.ActionAllow ||
		decision("file_write", tool.CapabilityWrite) != securitypolicy.ActionDeny {
		t.Fatal("delegating read-only role escaped its ordinary tool allowlist")
	}
}

func TestPersistentSessionPublishesAgentSpawnLive(t *testing.T) {
	workspace := t.TempDir()
	store, err := state.Open(t.Context(), state.Options{
		DataDir:     filepath.Join(t.TempDir(), "state"),
		BusyTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	tools := true
	session, err := NewExec(t.Context(), ExecOptions{
		FixturePath:     subagentFixture(t, "subagent"),
		Permission:      "bypass",
		PersistentStore: store,
		ConfigOverrides: config.Overrides{
			Tools: &tools, Workspace: &workspace,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = session.Close(context.Background())
		_ = store.CloseAll(context.Background())
	})
	cursor := session.Runtime.Snapshot(t.Context()).LastSequence
	events, err := session.Runtime.Events(t.Context(), cursor)
	if err != nil {
		t.Fatal(err)
	}
	child, err := session.subagents.Spawn("", subagent.RoleExplore, "inspect")
	if err != nil {
		t.Fatal(err)
	}
	resolvedWorkspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if child.Worktree != resolvedWorkspace && child.Worktree != workspace {
		t.Fatalf(
			"read-only child worktree = %q, want host workspace %q",
			child.Worktree,
			resolvedWorkspace,
		)
	}
	if strings.HasPrefix(child.Worktree, store.Root()+string(filepath.Separator)) {
		t.Fatalf(
			"persistent child worktree = %q leaked into state root %q",
			child.Worktree,
			store.Root(),
		)
	}
	select {
	case event := <-events:
		data, ok := event.Data.(*protocol.AgentSpawnedData)
		if !ok || data.AgentID != child.ID {
			t.Fatalf("event = %#v, want agent.spawned for %s", event, child.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("live agent.spawned event was not published")
	}
}

func TestNeverPostureRejectsPersistedWorkspaceAllow(t *testing.T) {
	workspace := t.TempDir()
	stateDataDir := t.TempDir()
	permissionPath, err := securitypolicy.PermissionsPath(stateDataDir, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(permissionPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(permissionPath, []byte(`
[[allow]]
tool = "file_write"
grant_key = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	tools := true
	session, err := NewExec(t.Context(), withNonDurableTestJournal(t, ExecOptions{
		FixturePath: subagentFixture(t, "openai"),
		Permission:  "never",
		ConfigOverrides: config.Overrides{
			Tools: &tools, Workspace: &workspace, StateDataDir: &stateDataDir,
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close(context.Background()) })
	call := resolvePolicyFixture(policyInvocationFixture{
		CallID: "untrusted-write", Tool: "file_write",
		Arguments: json.RawMessage(`{"path":"notes.txt","content":"blocked"}`),
		Resources: []tool.Resource{{
			Kind: "file", Path: "notes.txt", Access: tool.AccessWrite,
		}},
		Capability: tool.CapabilityWrite, Validated: true, Workspace: workspace,
	})
	decision := session.Security().Decide(call)
	if decision.Action != securitypolicy.ActionDeny || decision.Code != "permission_denied" {
		t.Fatalf("decision = %+v, want permission_denied", decision)
	}
}

// openChildSession builds a tools-enabled session against a subagent fixture.
// Each fixture serves exactly one stream, so the only provider request a test
// may produce is the child's — which is also the assertion that the child really
// talked to a model instead of returning placeholder text.
func openChildSession(
	t *testing.T, fixture string, tune func(*config.Overrides),
) *Session {
	return openChildSessionWithPermission(t, fixture, "bypass", tune)
}

func openChildSessionWithPermission(
	t *testing.T,
	fixture, permission string,
	tune func(*config.Overrides),
) *Session {
	t.Helper()
	workspace := t.TempDir()
	tools := true
	overrides := config.Overrides{Tools: &tools, Workspace: &workspace}
	if tune != nil {
		tune(&overrides)
	}
	session, err := NewExec(context.Background(), withNonDurableTestJournal(t, ExecOptions{
		FixturePath: subagentFixture(t, fixture), Permission: permission,
		ConfigOverrides: overrides,
	}))
	if err != nil {
		t.Fatalf("NewExec: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = session.Close(ctx)
	})
	if session.subagents == nil || session.children == nil {
		t.Fatal("session has no child agent runtime")
	}
	return session
}

// runChild spawns a child in the given role, starts its turn, and waits for the
// terminal result the child runtime settled.
func runChild(t *testing.T, session *Session, role subagent.Role) subagent.Result {
	t.Helper()
	manager := session.subagents
	child, err := manager.Spawn("", role, "count the packages")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if _, err := manager.Takeover(
		context.Background(), child.ID, "count the packages",
	); err != nil {
		t.Fatalf("Takeover: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	waited, err := manager.Wait(ctx, []string{child.ID}, 15*time.Second)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if waited.TimedOut {
		t.Fatal("child agent never reached a terminal status")
	}
	result, ok := manager.Result(child.ID)
	if !ok {
		t.Fatal("terminal child agent has no structured result")
	}
	return result
}

func unresolvedContains(result subagent.Result, fragment string) bool {
	for _, note := range result.Unresolved {
		if strings.Contains(note, fragment) {
			return true
		}
	}
	return false
}

func TestChildAgentRunsRealEngineTurn(t *testing.T) {
	session := openChildSession(t, "subagent", nil)
	manager := session.subagents

	cursor := session.Runtime.Snapshot(context.Background()).LastSequence
	events, err := session.Runtime.Events(context.Background(), cursor)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	child, err := manager.Spawn("", subagent.RoleExplore, "count the packages")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	turnID, err := manager.Takeover(context.Background(), child.ID, "count the packages")
	if err != nil {
		t.Fatalf("Takeover: %v", err)
	}
	if turnID == "" {
		t.Fatal("Takeover returned an empty turn id")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	waited, err := manager.Wait(ctx, []string{child.ID}, 10*time.Second)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if waited.TimedOut {
		t.Fatal("child agent never reached a terminal status")
	}

	result, ok := manager.Result(child.ID)
	if !ok {
		t.Fatal("terminal child agent has no structured result")
	}
	if result.Status != subagent.StatusCompleted {
		t.Fatalf("result = %+v", result)
	}
	if result.Summary != "the workspace has one package" {
		t.Fatalf("result summary = %q", result.Summary)
	}
	if result.TurnID != turnID || result.ThreadID != subagent.ThreadIDFor(child.ID) {
		t.Fatalf("result ids = %+v", result)
	}
	// Usage comes from the child's own receipt, so it proves the child was
	// accounted for rather than reported as an unknown.
	if result.Usage.InputTokens != 11 || result.Usage.OutputTokens != 6 {
		t.Fatalf("result usage = %+v", result.Usage)
	}
	// A read-only child changes nothing, so the gate has nothing to verify. That
	// must read as not_evaluated, never as passed.
	if result.Verification.Verify != protocol.ReceiptNotEvaluated {
		t.Fatalf("result verification = %+v", result.Verification)
	}
	if len(result.Diff) != 0 {
		t.Fatalf("read-only child reported changes: %+v", result.Diff)
	}

	// The child's turn must be visible in the event stream under its own thread,
	// which is what makes it auditable and replayable like any other turn.
	childThread := protocol.ThreadID(subagent.ThreadIDFor(child.ID))
	sawStarted, sawReceipt, sawCompleted := false, false, false
	deadline := time.After(5 * time.Second)
	for !sawStarted || !sawReceipt || !sawCompleted {
		select {
		case event, open := <-events:
			if !open {
				t.Fatal("event stream closed before the child turn was observed")
			}
			if event.ThreadID != childThread {
				continue
			}
			switch event.Kind {
			case protocol.EventTurnStarted:
				started, _ := event.Data.(*protocol.TurnStartedData)
				if started == nil {
					t.Fatal("child turn.started payload is missing")
				}
				sawStarted = true
			case protocol.EventExecutionReceipt:
				receipt, _ := event.Data.(*protocol.ExecutionReceiptData)
				if receipt == nil {
					t.Fatal("child turn receipt is missing")
				}
				sawReceipt = true
			case protocol.EventTurnCompleted:
				sawCompleted = true
			}
		case <-deadline:
			t.Fatalf(
				"child thread events missing: started=%v receipt=%v completed=%v",
				sawStarted,
				sawReceipt,
				sawCompleted,
			)
		}
	}
}

func TestChildFollowUpReservesOnlyRemainingAgentBudget(t *testing.T) {
	budget := uint64(20_000)
	session := openChildSession(t, "subagent-followup", func(overrides *config.Overrides) {
		overrides.SubagentMaxTokens = &budget
	})
	manager := session.subagents
	child, err := manager.SpawnIntent(subagent.DelegationIntent{
		TaskName: "remaining_budget", Role: subagent.RoleExplore,
		Objective: "count the packages", ExpectedOutput: "count",
		Trigger: subagent.TriggerUser,
		Budget:  subagent.AgentBudget{MaxTokens: budget},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Takeover(
		t.Context(), child.ID, "count the packages",
	); err != nil {
		t.Fatal(err)
	}
	waitForAgentStatus(t, manager, child.ID, subagent.StatusCompleted)

	if _, err := manager.FollowUp(
		t.Context(), child.ID, "count the packages again",
	); err != nil {
		t.Fatalf("FollowUp with remaining lifecycle budget: %v", err)
	}
	waitForAgentStatus(t, manager, child.ID, subagent.StatusCompleted)

	settled, ok := manager.Agent(child.ID)
	if !ok || settled.SpentTokens != 34 || settled.ReservedTokens != 0 {
		t.Fatalf("follow-up Agent budget = %+v", settled)
	}
	if ledger := manager.SessionBudget(child.SessionID); ledger.SpentTokens != 34 ||
		ledger.ReservedTokens != 0 || ledger.ReservedSlots != 0 {
		t.Fatalf("follow-up tree budget = %+v", ledger)
	}
}

func TestChildResidencyLRUUnloadAndOnDemandRestore(t *testing.T) {
	session := openChildSession(t, "subagent", func(overrides *config.Overrides) {
		parallel, resident, total := 1, 1, 3
		overrides.SubagentMaxParallel = &parallel
		overrides.SubagentMaxResident = &resident
		overrides.SubagentMaxTotal = &total
	})
	manager := session.subagents
	first, err := manager.Spawn("", subagent.RoleExplore, "count the packages")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Takeover(
		t.Context(),
		first.ID,
		"count the packages",
	); err != nil {
		t.Fatal(err)
	}
	waitForAgentStatus(t, manager, first.ID, subagent.StatusCompleted)
	firstThread := protocol.ThreadID(first.ThreadID)
	firstSpec, ok := session.threads.ChildSpecFor(firstThread)
	if !ok {
		t.Fatal("first child was not resident after completion")
	}
	before, _ := manager.Agent(first.ID)
	if _, err := manager.Mailbox().Enqueue(subagent.Message{
		SessionID: first.SessionID, From: "parent", To: first.ID,
		Kind: subagent.MessageContext, Body: json.RawMessage(`{"note":"resume"}`),
	}); err != nil {
		t.Fatal(err)
	}

	second, err := manager.Spawn("", subagent.RoleExplore, "count the packages")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Takeover(
		t.Context(),
		second.ID,
		"count the packages",
	); err != nil {
		t.Fatal(err)
	}
	waited, err := manager.Wait(t.Context(), []string{second.ID}, 5*time.Second)
	if err != nil || waited.TimedOut {
		t.Fatalf("second child wait = %+v, err=%v", waited, err)
	}
	if _, ok := session.threads.ChildSpecFor(firstThread); ok {
		t.Fatal("LRU child thread remained resident")
	}
	firstUnloaded, _ := manager.Agent(first.ID)
	if firstUnloaded.Resident {
		t.Fatalf("first Agent remained resident: %+v", firstUnloaded)
	}

	if _, err := manager.FollowUp(
		t.Context(),
		first.ID,
		"count the packages",
	); err != nil {
		t.Fatal(err)
	}
	reloadedSpec, ok := session.threads.ChildSpecFor(firstThread)
	if !ok {
		t.Fatal("first child thread was not restored on demand")
	}
	if reloadedSpec.Role != firstSpec.Role ||
		reloadedSpec.Stance != firstSpec.Stance ||
		reloadedSpec.Workspace != firstSpec.Workspace ||
		strings.Join(reloadedSpec.AllowedTools, "\x00") !=
			strings.Join(firstSpec.AllowedTools, "\x00") {
		t.Fatalf("restored authority changed: before=%+v after=%+v", firstSpec, reloadedSpec)
	}
	after, _ := manager.Agent(first.ID)
	if before.Context != nil && (after.Context == nil ||
		after.Context.Digest != before.Context.Digest) {
		t.Fatalf("restored Context changed: before=%+v after=%+v", before.Context, after.Context)
	}
	if pending := manager.Mailbox().PendingSession(first.SessionID, first.ID); len(pending) != 0 {
		t.Fatalf("restored mailbox retained delivered messages: %+v", pending)
	}
	secondThread := protocol.ThreadID(second.ThreadID)
	if _, ok := session.threads.ChildSpecFor(secondThread); ok {
		t.Fatal("second LRU child thread remained resident")
	}
	secondUnloaded, _ := manager.Agent(second.ID)
	if secondUnloaded.Resident {
		t.Fatalf("second Agent remained resident: %+v", secondUnloaded)
	}
	waited, err = manager.Wait(t.Context(), []string{first.ID}, 5*time.Second)
	if err != nil || waited.TimedOut {
		t.Fatalf("restored child wait = %+v, err=%v", waited, err)
	}
}

func TestSuggestChildWaitsForHostApprovalWithSource(t *testing.T) {
	session, manager, child, events, required := startSuggestChildApproval(t)
	if required.Source == nil ||
		required.Source.AgentID != child.ID ||
		required.Source.AgentPath != child.Path ||
		required.Source.ParentPath != child.ParentPath ||
		required.Source.Role != string(child.Role) ||
		required.Source.SessionID == "" ||
		required.Source.WorkspaceRoot == "" {
		t.Fatalf("approval source = %+v, child = %+v", required.Source, child)
	}
	waitForAgentStatus(t, manager, child.ID, subagent.StatusWaiting)
	submitChildApproval(t, session, child, required, protocol.ApprovalApprove)
	resolved := waitForChildApprovalResolved(t, events, child.ThreadID)
	if resolved.RequestID != required.RequestID || resolved.Problem != nil {
		t.Fatalf("resolved approval = %+v, required = %+v", resolved, required)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	waited, err := manager.Wait(ctx, []string{child.ID}, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if waited.TimedOut {
		t.Fatal("approved child did not finish")
	}
	result, ok := manager.Result(child.ID)
	if !ok || result.Status != subagent.StatusCompleted {
		t.Fatalf("approved child result = %+v, ok=%v", result, ok)
	}
}

func TestDeniedChildApprovalPublishesProblemAndToolFeedback(t *testing.T) {
	session, _, child, events, required := startSuggestChildApproval(t)
	submitChildApproval(t, session, child, required, protocol.ApprovalDeny)

	deadline := time.After(10 * time.Second)
	sawProblem, sawFeedback := false, false
	for !sawProblem || !sawFeedback {
		select {
		case event := <-events:
			if event.ThreadID != protocol.ThreadID(child.ThreadID) {
				continue
			}
			switch data := event.Data.(type) {
			case *protocol.ApprovalResolvedData:
				sawProblem = data.RequestID == required.RequestID &&
					data.Problem != nil &&
					data.Problem.Details != nil &&
					data.Problem.Details.Reason == "approval_denied"
			case *protocol.ToolResultData:
				if data.CallID == required.CallID && data.IsError &&
					strings.Contains(strings.ToLower(data.Output), "denied") {
					sawFeedback = true
				}
			}
		case <-deadline:
			t.Fatalf(
				"denied approval evidence missing: problem=%v feedback=%v",
				sawProblem, sawFeedback,
			)
		}
	}
}

func TestChildCancelPendingApprovalPublishesOneTerminal(t *testing.T) {
	session, manager, child, events, _ := startSuggestChildApproval(t)
	if _, err := manager.Interrupt(
		t.Context(), child.ID,
	); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	waited, err := manager.Wait(ctx, []string{child.ID}, 8*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if waited.TimedOut {
		t.Fatal("canceled approval Child did not settle")
	}
	result, ok := manager.Result(child.ID)
	if !ok || result.Status != subagent.StatusInterrupted {
		t.Fatalf("canceled approval Result = %+v, ok=%v", result, ok)
	}
	if _, err := manager.Interrupt(t.Context(), child.ID); err != nil {
		t.Fatalf("repeated interrupt after settlement: %v", err)
	}
	if messages := manager.Mailbox().PendingSession(child.SessionID, subagent.SessionParentID); len(messages) != 1 ||
		messages[0].Kind != subagent.MessageCompletion {
		t.Fatalf("cancel completion messages = %+v", messages)
	}
	deadline := time.After(5 * time.Second)
	terminals := 0
	for terminals == 0 {
		select {
		case event := <-events:
			if event.ThreadID == protocol.ThreadID(child.ThreadID) &&
				event.Kind == protocol.EventTurnCanceled {
				terminals++
			}
		case <-deadline:
			t.Fatal("approval Child did not publish turn.canceled")
		}
	}
	time.Sleep(50 * time.Millisecond)
	if snapshot := session.Runtime.Snapshot(t.Context()); snapshot.ActiveTurns != 0 || snapshot.PendingApprovals != 0 {
		t.Fatalf("Runtime Snapshot after approval cancel = %+v", snapshot)
	}
}

func startSuggestChildApproval(
	t *testing.T,
) (*Session, *subagent.AgentControl, subagent.Agent, <-chan protocol.Event, *protocol.ApprovalRequiredData) {
	return startSuggestChildApprovalWithTune(t, nil)
}

func startSuggestChildApprovalWithTune(
	t *testing.T,
	tune func(*config.Overrides),
) (*Session, *subagent.AgentControl, subagent.Agent, <-chan protocol.Event, *protocol.ApprovalRequiredData) {
	t.Helper()
	session := openChildSessionWithPermission(
		t, "subagent-write", "suggest",
		func(overrides *config.Overrides) {
			serialized := config.SubagentWorkspaceSerialized
			overrides.SubagentWorkspace = &serialized
			if tune != nil {
				tune(overrides)
			}
		},
	)
	// Approval proxy tests deliberately tighten this fixture call. Ordinary
	// journaled file edits are low risk under suggest posture in A1.
	session.Security().Repository = append([]securitypolicy.Rule{{
		Tool: "file_write", Resource: "*", Action: securitypolicy.ActionAsk,
	}}, session.Security().Repository...)
	manager := session.subagents
	cursor := session.Runtime.Snapshot(t.Context()).LastSequence
	events, err := session.Runtime.Events(t.Context(), cursor)
	if err != nil {
		t.Fatal(err)
	}
	child, err := manager.Spawn("", subagent.RoleImplementer, "write and verify child-note.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Takeover(
		t.Context(), child.ID, "write and verify child-note.txt",
	); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(10 * time.Second)
	for {
		select {
		case event := <-events:
			if event.ThreadID != protocol.ThreadID(child.ThreadID) {
				continue
			}
			if required, ok := event.Data.(*protocol.ApprovalRequiredData); ok {
				current, _ := manager.Agent(child.ID)
				return session, manager, current, events, required
			}
		case <-deadline:
			t.Fatal("writing child did not request approval under suggest posture")
		}
	}
}

func submitChildApproval(
	t *testing.T,
	session *Session,
	child subagent.Agent,
	required *protocol.ApprovalRequiredData,
	decision protocol.ApprovalDecision,
) {
	t.Helper()
	itemID, err := protocol.NewItemID()
	if err != nil {
		t.Fatal(err)
	}
	payload := &protocol.ApprovalDecisionPayload{
		ThreadID:  protocol.ThreadID(child.ThreadID),
		TurnID:    protocol.TurnID(child.TurnID),
		ItemID:    itemID,
		RequestID: required.RequestID,
		Decision:  decision,
		Scope:     protocol.ApprovalScopeOnce,
	}
	if decision == protocol.ApprovalApprove && required.EditPlan != nil {
		payload.PlanID = required.EditPlan.ID
	}
	operation, err := protocol.NewOperation(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Runtime.Submit(t.Context(), operation); err != nil {
		t.Fatal(err)
	}
}

func waitForAgentStatus(
	t *testing.T,
	manager *subagent.AgentControl,
	agentID string,
	status subagent.Status,
) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if agent, ok := manager.Agent(agentID); ok && agent.Status == status {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	agent, _ := manager.Agent(agentID)
	t.Fatalf("agent status = %q, want %q", agent.Status, status)
}

func waitForChildApprovalResolved(
	t *testing.T,
	events <-chan protocol.Event,
	threadID string,
) *protocol.ApprovalResolvedData {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case event := <-events:
			if event.ThreadID != protocol.ThreadID(threadID) {
				continue
			}
			if resolved, ok := event.Data.(*protocol.ApprovalResolvedData); ok {
				return resolved
			}
		case <-deadline:
			t.Fatal("child approval was not resolved")
		}
	}
}

// TestChildAgentWithWritingStanceIsRejectedAtTakeover covers the second gate: an
// agent that reached the runtime without an isolated root must not run, even
// though the spawn path already refuses to create one.
func TestChildAgentWithWritingStanceIsRejectedAtTakeover(t *testing.T) {
	session := openChildSession(t, "subagent", nil)
	manager := session.subagents

	// Spawn as explore (isolation is not needed to create it), then ask the child
	// runtime to run it as a writing agent.
	child, err := manager.Spawn("", subagent.RoleExplore, "edit the file")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	snapshot, _ := manager.Agent(child.ID)
	snapshot.Stance = subagent.StanceWrite
	if _, err := session.children.Spec(snapshot); err == nil {
		t.Fatal("a writing child agent must not run against the parent workspace")
	} else if !protocol.IsCode(err, protocol.CodeUnavailable) {
		t.Fatalf("specFor error = %v (want unavailable)", err)
	}
}

func TestChildAgentReadOnlyOverrideRunsWritingStance(t *testing.T) {
	// An operator who forces read_only accepts that a writing stance runs without
	// write access; the child still executes rather than being rejected.
	session := openChildSession(t, "subagent", func(overrides *config.Overrides) {
		readOnly := config.SubagentWorkspaceReadOnly
		overrides.SubagentWorkspace = &readOnly
	})
	if result := runChild(t, session, subagent.RoleImplementer); result.Status != subagent.StatusCompleted {
		t.Fatalf("result = %+v", result)
	}
}

func TestChildAgentProgressLeaseUsesReservedFinalization(t *testing.T) {
	// The child's progress lease is its own, not the parent's. A new read renews
	// it; one subsequent no-progress Sample exhausts it and reserves a
	// finalization Sample so the child can close structurally.
	session := openChildSession(t, "subagent-steps", func(overrides *config.Overrides) {
		steps := 1
		overrides.SubagentMaxSteps = &steps
	})
	result := runChild(t, session, subagent.RoleExplore)
	if result.Status != subagent.StatusCompleted ||
		!strings.Contains(result.Summary, "work-step budget ended") {
		t.Fatalf("result = %+v", result)
	}
}

func TestChildAgentIdleLeaseInterruptsTurnRecoverably(t *testing.T) {
	// The explicit child lease is short enough to expire between observable
	// progress events. Expiry interrupts the child so a later takeover can
	// continue it instead of terminalizing it as a permanent failure.
	session := openChildSession(t, "subagent-slow", func(overrides *config.Overrides) {
		wallTime := 50 * time.Millisecond
		overrides.SubagentWallTime = &wallTime
	})
	result := runChild(t, session, subagent.RoleExplore)
	if result.Status != subagent.StatusInterrupted {
		t.Fatalf("status = %q, want interrupted: %+v", result.Status, result)
	}
	if !unresolvedContains(result, "execution lease expired") {
		t.Fatalf("unresolved = %v", result.Unresolved)
	}
}

func TestReleaseCompletesFromRunningChildTerminalEvent(t *testing.T) {
	session := openChildSession(t, "subagent-slow", nil)
	child, err := session.subagents.Spawn(
		"", subagent.RoleExplore, "wait until canceled",
	)
	if err != nil {
		t.Fatal(err)
	}
	turnID, err := session.subagents.Takeover(
		t.Context(), child.ID, "wait until canceled",
	)
	if err != nil {
		t.Fatal(err)
	}
	threadID := protocol.ThreadID(subagent.ThreadIDFor(child.ID))
	if _, ok := session.threads.ChildSpecFor(threadID); !ok {
		t.Fatal("child spec was not registered")
	}
	running, tracked := session.children.Tracked(threadID)
	if !tracked {
		t.Fatal("running child turn was not tracked")
	}
	events, err := session.Runtime.Events(
		t.Context(),
		session.Runtime.Snapshot(t.Context()).LastSequence,
	)
	if err != nil {
		t.Fatal(err)
	}
	session.children.Release(child.ID)
	deadline := time.After(5 * time.Second)
	for {
		select {
		case event := <-events:
			if event.TurnID != protocol.TurnID(turnID) ||
				event.Kind != protocol.EventTurnCanceled {
				continue
			}
			select {
			case <-running.Terminal:
			case <-time.After(time.Second):
				t.Fatal("canceled child settlement did not finish")
			}
			if _, ok := session.threads.ChildSpecFor(threadID); ok {
				t.Fatal("child spec remained after cancellation reached terminal")
			}
			return
		case <-deadline:
			t.Fatal("child cancellation did not reach a terminal event")
		}
	}
}

func TestChildAgentSpendIsChargedToTheSharedLedger(t *testing.T) {
	session := openChildSession(t, "subagent", nil)
	result := runChild(t, session, subagent.RoleExplore)
	if result.Status != subagent.StatusCompleted {
		t.Fatalf("result = %+v", result)
	}
	agent, ok := session.subagents.Agent(result.AgentID)
	if !ok {
		t.Fatal("settled Agent is unavailable")
	}
	// The fixture reports 11 input and 6 output tokens. The ledger must carry
	// exactly that: a placeholder charge at spawn time would show up here as an
	// extra token, and no charge at all would leave the budget unenforceable.
	// The turn's slot and reservation are held until settlement and must be back.
	ledger := session.subagents.SessionBudget(agent.SessionID)
	if ledger.SpentTokens != 17 || ledger.ReservedTokens != 0 ||
		ledger.ReservedMicros != 0 || ledger.ReservedSlots != 0 {
		t.Fatalf("tree ledger after settle = %+v", ledger)
	}
}

func TestChildAgentTurnHoldsItsConcurrencySlotWhileRunning(t *testing.T) {
	// The slow fixture keeps the child mid-turn long enough to observe the slot.
	session := openChildSession(t, "subagent-slow", nil)
	manager := session.subagents
	child, err := manager.Spawn("", subagent.RoleExplore, "count the packages")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if _, err := manager.Takeover(
		context.Background(), child.ID, "count the packages",
	); err != nil {
		t.Fatalf("Takeover: %v", err)
	}
	if slots := manager.SessionBudget(child.SessionID).ReservedSlots; slots != 1 {
		t.Fatalf("slots during a running child turn = %d, want 1", slots)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if waited, err := manager.Wait(ctx, []string{child.ID}, 15*time.Second); err != nil {
		t.Fatalf("Wait: %v", err)
	} else if waited.TimedOut {
		t.Fatal("child agent never reached a terminal status")
	}
	if slots := manager.SessionBudget(child.SessionID).ReservedSlots; slots != 0 {
		t.Fatalf("slots after settle = %d", slots)
	}
}

func TestChildConcurrencyIsScopedToItsSessionTree(t *testing.T) {
	session := openChildSession(t, "subagent-slow", func(overrides *config.Overrides) {
		parallel := 1
		overrides.SubagentMaxParallel = &parallel
	})
	manager := session.subagents
	running, err := manager.Spawn("", subagent.RoleExplore, "count the packages")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Takeover(t.Context(), running.ID, "count the packages"); err != nil {
		t.Fatal(err)
	}
	other, err := manager.SpawnIntent(subagent.DelegationIntent{
		SessionID: "session-other", TaskName: "other_tree",
		Role: subagent.RoleExplore, Objective: "count the packages",
		ExpectedOutput: "count", Trigger: subagent.TriggerUser,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Takeover(t.Context(), other.ID, "count the packages"); err != nil {
		t.Fatalf("another Session's running child consumed this tree's concurrency: %v", err)
	}
	sibling, err := manager.Spawn("", subagent.RoleExplore, "count the packages")
	if err != nil {
		t.Fatal(err)
	}
	_, err = manager.Takeover(t.Context(), sibling.ID, "count the packages")
	var problem *protocol.Problem
	if !errors.As(err, &problem) ||
		problem.Code != protocol.CodeResourceExhausted || !problem.Retryable ||
		problem.Details == nil ||
		problem.Details.Reason != subagent.ReasonConcurrencyExhausted {
		t.Fatalf("same-tree concurrency refusal = %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	if _, err := manager.Wait(ctx, []string{running.ID, other.ID}, 15*time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestChildAgentRefusedWhenTheTreeBudgetIsCommitted(t *testing.T) {
	budget := uint64(5000)
	session := openChildSession(t, "subagent-slow", func(overrides *config.Overrides) {
		overrides.SubagentMaxTokens = &budget
	})
	manager := session.subagents
	whole, err := manager.SpawnIntent(subagent.DelegationIntent{
		TaskName: "whole_tree", Role: subagent.RoleExplore,
		Objective: "count the packages", ExpectedOutput: "count",
		Trigger: subagent.TriggerUser,
		Budget:  subagent.AgentBudget{MaxTokens: budget},
	})
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := manager.Spawn("", subagent.RoleExplore, "count the packages")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Takeover(t.Context(), whole.ID, "count the packages"); err != nil {
		t.Fatal(err)
	}
	_, err = manager.Takeover(t.Context(), waiting.ID, "count the packages")
	if !protocol.IsCode(err, protocol.CodeResourceExhausted) {
		t.Fatalf("Takeover error = %v (want resource_exhausted)", err)
	}
	var problem *protocol.Problem
	if !errors.As(err, &problem) ||
		problem.Retryable ||
		problem.Fault == nil ||
		problem.Fault.Disposition != protocol.FaultResumeTurn ||
		problem.Details == nil ||
		problem.Details.Reason != protocol.ProblemReasonTokenBudgetExhausted ||
		problem.Details.ResourceID != "agent_tree:"+waiting.SessionID {
		t.Fatalf("child budget error = %+v", problem)
	}
	// Refused before submission means no turn was consumed and no slot leaked.
	if slots := manager.SessionBudget(waiting.SessionID).ReservedSlots; slots != 1 {
		t.Fatalf("slots after refusal = %d, want only the running child", slots)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	if _, err := manager.Wait(ctx, []string{whole.ID}, 15*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Takeover(t.Context(), waiting.ID, "count the packages"); err != nil {
		t.Fatalf("a settled reservation was not returned to the tree: %v", err)
	}
}
