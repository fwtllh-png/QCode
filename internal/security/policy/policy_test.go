package policy

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
)

func TestRuntimePermissionUpdateIsAtomic(t *testing.T) {
	runtime := DefaultRuntime(ModeAct, PermissionNever)
	var wait sync.WaitGroup
	wait.Add(2)
	invalid := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer wait.Done()
		for range 10_000 {
			runtime.SetPermission(PermissionAuto)
			runtime.SetPermission(PermissionNever)
		}
		close(done)
	}()
	go func() {
		defer wait.Done()
		for {
			snapshot := runtime.CloneSampling()
			if snapshot.Mode != ModeAct ||
				(snapshot.Permission != PermissionAuto && snapshot.Permission != PermissionNever) {
				select {
				case invalid <- struct{}{}:
				default:
				}
				return
			}
			select {
			case <-done:
				return
			default:
			}
		}
	}()
	wait.Wait()
	select {
	case <-invalid:
		t.Fatal("observed a mixed policy update")
	default:
	}
}

func TestRuntimePermissionCeilingUsesCurrentValueUnderLock(t *testing.T) {
	runtime := DefaultRuntime(ModeAct, PermissionSuggest)

	runtime.SetPermission(PermissionNever)
	runtime.SetPermissionWithinCeiling(PermissionBypass, "")
	snapshot := runtime.CloneSampling()
	if snapshot.Mode != ModeAct {
		t.Fatalf("mode = %q, want %q", snapshot.Mode, ModeAct)
	}
	if snapshot.Permission != PermissionNever {
		t.Fatalf("permission = %q, want revoked ceiling %q", snapshot.Permission, PermissionNever)
	}

	runtime.SetPermission(PermissionAuto)
	runtime.SetPermissionWithinCeiling(PermissionBypass, PermissionSuggest)
	if got := runtime.PermissionValue(); got != PermissionSuggest {
		t.Fatalf("permission = %q, want explicit ceiling %q", got, PermissionSuggest)
	}
}

func TestPolicyTruthTableAndDenyPrecedence(t *testing.T) {
	now := time.Unix(1000, 0)
	tests := []struct {
		name       string
		mode       Mode
		permission Permission
		tool       string
		wantCode   string
	}{
		{name: "act read", mode: ModeAct, permission: PermissionSuggest, tool: "file_read"},
		{name: "act request_user_input", mode: ModeAct, permission: PermissionSuggest, tool: "request_user_input"},
		{name: "act session state allowed", mode: ModeAct, permission: PermissionSuggest, tool: "submit_plan"},
		{name: "act auto write", mode: ModeAct, permission: PermissionAuto, tool: "file_write"},
		{name: "act auto read-only shell", mode: ModeAct, permission: PermissionAuto, tool: "shell_read"},
		{name: "act auto sandboxed process", mode: ModeAct, permission: PermissionAuto, tool: "exec_command"},
		{name: "removed plan rejected", mode: "plan", permission: PermissionBypass, tool: "file_write", wantCode: "mode_unknown"},
		{name: "removed operate rejected", mode: "operate", permission: PermissionBypass, tool: "file_read", wantCode: "mode_unknown"},
		{name: "never write denied", mode: ModeAct, permission: PermissionNever, tool: "file_write", wantCode: "permission_denied"},
		{name: "unknown denied", mode: ModeAct, permission: PermissionBypass, tool: "future_tool", wantCode: "policy_unknown_capability"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runtime := DefaultRuntime(test.mode, test.permission)
			runtime.Now = func() time.Time { return now }
			raw := `{}`
			if test.tool == "file_write" {
				raw = `{"path":"notes.txt"}`
			}
			err := authorize(runtime, invocation(test.tool, "call-1", raw))
			assertDecisionCode(t, err, test.wantCode)
		})
	}

	t.Run("act auto external asks", func(t *testing.T) {
		runtime := DefaultRuntime(ModeAct, PermissionAuto)
		call := invocation("file_read", "external-1", `{}`)
		call.Capability = CapabilityExternal
		err := authorize(runtime, call)
		assertDecisionCode(t, err, "approval_required")
	})
	t.Run("act auto network read asks", func(t *testing.T) {
		runtime := DefaultRuntime(ModeAct, PermissionAuto)
		call := invocation("file_read", "net-act", `{}`)
		call.Capability = CapabilityNetwork
		err := authorize(runtime, call)
		assertDecisionCode(t, err, "approval_required")
	})
	runtime := DefaultRuntime(ModeAct, PermissionBypass)
	runtime.Repository = []Rule{
		{Tool: "file_write", Resource: "protected", Action: ActionDeny},
		{Tool: "file_write", Resource: "protected", Action: ActionAllow},
	}
	err := authorize(runtime, invocation("file_write", "call-2", `{"path":"protected/value"}`))
	assertDecisionCode(t, err, "repository_rule_denied")

	runtime.Repository = []Rule{{Tool: "file_write", Action: ActionHold, Code: "release_hold"}}
	err = authorize(runtime, invocation("file_write", "call-3", `{"path":"value"}`))
	assertDecisionCode(t, err, "release_hold")

	runtime = DefaultRuntime(ModeAct, PermissionSuggest)
	runtime.User = []Rule{{Tool: "file_write", Resource: "notes.txt", Action: ActionAllow}}
	err = authorize(runtime, invocation("file_write", "call-allow", `{"path":"notes.txt"}`))
	assertDecisionCode(t, err, "")

	runtime = DefaultRuntime(ModeAct, PermissionNever)
	runtime.User = []Rule{{Tool: "file_write", Resource: "notes.txt", Action: ActionAllow}}
	err = authorize(runtime, invocation("file_write", "call-never", `{"path":"notes.txt"}`))
	assertDecisionCode(t, err, "permission_denied")

	runtime = DefaultRuntime(ModeAct, PermissionSuggest)
	err = authorize(runtime, invocation(
		"file_write", "call-auto-write", `{"path":"notes.txt","content":"done"}`,
	))
	assertDecisionCode(t, err, "")
	edit := invocation("file_edit", "call-edit", `{"path":"notes.txt"}`)
	edit.Capability = CapabilityWrite
	err = authorize(runtime, edit)
	assertDecisionCode(t, err, "")
	runtime.Repository = []Rule{{
		Tool: "file_write", Resource: "notes.txt", Action: ActionAsk,
	}}
	err = authorize(runtime, invocation(
		"file_write", "call-repository-ask", `{"path":"notes.txt","content":"done"}`,
	))
	assertDecisionCode(t, err, "approval_required")
}

func TestCloneSamplingIsolatesPermissionAndRules(t *testing.T) {
	parent := DefaultRuntime(ModeAct, PermissionSuggest)
	parent.Repository = []Rule{{Tool: "write", Resource: "*", Action: ActionAsk}}
	clone := parent.CloneSampling()
	if clone == nil || clone == parent {
		t.Fatal("clone must be a distinct runtime")
	}
	if clone.Approvals != parent.Approvals {
		t.Fatal("approvals cache should be shared")
	}
	parent.Permission = PermissionBypass
	parent.Repository[0].Action = ActionDeny
	if clone.Mode != ModeAct || clone.Permission != PermissionSuggest {
		t.Fatalf("clone mode/permission = %s/%s", clone.Mode, clone.Permission)
	}
	if clone.Repository[0].Action != ActionAsk {
		t.Fatal("repository rules must be copied")
	}
}

func TestPolicyCompletePermissionCapabilityTruthTable(t *testing.T) {
	permissions := []Permission{
		PermissionSuggest, PermissionAuto, PermissionBypass, PermissionNever,
	}
	capabilities := []Capability{
		CapabilityRead, CapabilityWrite, CapabilityProcess, CapabilityNetwork, CapabilityExternal,
	}
	for _, permission := range permissions {
		for _, capability := range capabilities {
			name := string(permission) + "/" + string(capability)
			t.Run(name, func(t *testing.T) {
				runtime := DefaultRuntime(ModeAct, permission)
				call := invocation("file_read", "truth-table", `{}`)
				call.Capability = capability
				call.Access, call.Sandbox = "", ""
				err := authorize(runtime, call)
				want := ""
				switch {
				case permission == PermissionSuggest && capability != CapabilityRead:
					want = "approval_required"
				case permission == PermissionAuto:
					switch capability {
					case CapabilityRead:
						want = ""
					case CapabilityWrite, CapabilityProcess, CapabilityNetwork, CapabilityExternal:
						want = "approval_required"
					default:
						want = "permission_denied"
					}
				case permission == PermissionNever && capability != CapabilityRead:
					want = "permission_denied"
				}
				assertDecisionCode(t, err, want)
			})
		}
	}
}

func TestRepositoryCommandRulesInspectEveryShellSegment(t *testing.T) {
	for _, command := range []string{
		"rm target",
		"echo safe; rm target",
		"echo safe && rm target",
		"echo safe || rm target",
		"printf safe | rm target",
		"echo safe\nrm target",
	} {
		runtime := DefaultRuntime(ModeAct, PermissionBypass)
		runtime.Repository = []Rule{
			{Tool: "exec_command", CommandPrefix: "rm", Action: ActionDeny},
		}
		raw, err := json.Marshal(map[string]string{"command": command})
		if err != nil {
			t.Fatal(err)
		}
		err = authorize(runtime, invocation("exec_command", command, string(raw)))
		assertDecisionCode(t, err, "repository_rule_denied")
	}

	runtime := DefaultRuntime(ModeAct, PermissionBypass)
	runtime.Repository = []Rule{
		{Tool: "exec_command", CommandPrefix: "rm", Action: ActionDeny},
	}
	err := authorize(runtime, invocation(
		"exec_command", "quoted", `{"command":"printf '%s' 'echo safe; rm target'"}`,
	))
	assertDecisionCode(t, err, "")
}

func TestToolGrantMissingAndDenyCannotBeOverridden(t *testing.T) {
	runtime := DefaultRuntime(ModeAct, PermissionBypass)
	runtime.Grants = []Rule{{Tool: "file_read", Action: ActionAllow}}
	err := authorize(runtime, invocation("file_write", "call-1", `{"path":"a"}`))
	assertDecisionCode(t, err, "tool_grant_missing")

	runtime.Grants = []Rule{
		{Tool: "file_write", Resource: "*", Action: ActionAllow},
		{Tool: "file_write", Resource: "a", Action: ActionDeny},
	}
	err = authorize(runtime, invocation("file_write", "call-2", `{"path":"a"}`))
	assertDecisionCode(t, err, "tool_grant_denied")
}

func TestApprovalIsBoundToCallArgumentsResourcesScopeAndExpiry(t *testing.T) {
	now := time.Unix(2000, 0)
	base := invocation("exec_command", "call-1", `{"cwd":".","command":"go test ./..."}`)
	request, err := NewApprovalRequest(base, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	reorderedCall := invocation(
		"exec_command", "call-1", `{"command":"go test ./...","cwd":"."}`,
	)
	reordered, err := NewApprovalRequest(reorderedCall, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if request.Fingerprint != reordered.Fingerprint {
		t.Fatalf("canonical fingerprints differ: %s != %s", request.Fingerprint, reordered.Fingerprint)
	}
	differentExpiry, err := NewApprovalRequest(base, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if request.Fingerprint == differentExpiry.Fingerprint {
		t.Fatal("expiry change retained approval fingerprint")
	}

	cache := NewApprovalCache()
	if err := cache.Add(request, ApprovalOnce); err != nil {
		t.Fatal(err)
	}
	if !cache.MatchInvocation(reorderedCall, now) ||
		cache.MatchInvocation(reorderedCall, now) {
		t.Fatal("once approval was not consumed exactly once")
	}
	sessionRequest, err := NewApprovalRequestForScope(base, ApprovalSession, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Add(sessionRequest, ApprovalSession); err != nil {
		t.Fatal(err)
	}
	if !cache.MatchInvocation(base, now) ||
		!cache.MatchInvocation(base, now.Add(30*time.Second)) {
		t.Fatal("session approval was not reusable before expiry")
	}
	if cache.MatchInvocation(base, now.Add(2*time.Minute)) {
		t.Fatal("expired approval matched")
	}

	for _, changed := range []Invocation{
		invocation("exec_command", "call-1", `{"cwd":".","command":"rm -rf ."}`),
		invocation("file_write", "call-1", `{"path":"."}`),
	} {
		other, err := NewApprovalRequest(changed, now.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if other.Fingerprint == request.Fingerprint {
			t.Fatalf("changed invocation retained fingerprint: %+v", changed)
		}
	}
}

func TestSessionApprovalWithoutExpiryLivesWithPolicyRuntime(t *testing.T) {
	now := time.Unix(2000, 0)
	call := invocation(
		"exec_command",
		"call-session",
		`{"cwd":".","command":"go test ./..."}`,
	)
	request, err := NewApprovalRequestForScope(
		call,
		ApprovalSession,
		time.Time{},
	)
	if err != nil {
		t.Fatal(err)
	}
	cache := NewApprovalCache()
	if err := cache.Add(request, ApprovalSession); err != nil {
		t.Fatal(err)
	}
	if !cache.MatchInvocation(call, now) ||
		!cache.MatchInvocation(call, now.Add(365*24*time.Hour)) {
		t.Fatal("session approval unexpectedly expired inside its policy runtime")
	}
}

func TestSuggestLowRiskEditDoesNotRequireAsyncHost(t *testing.T) {
	runtime := DefaultRuntime(ModeAct, PermissionSuggest)
	call := invocation("file_edit", "call-approval", `{"path":"a"}`)
	call.Capability = CapabilityWrite
	assertDecisionCode(t, authorize(runtime, call), "")
}

func TestTightenPermissionNeverExpandsAuthority(t *testing.T) {
	permissions := []Permission{
		PermissionNever,
		PermissionSuggest,
		PermissionAuto,
		PermissionBypass,
	}
	for ceilingIndex, ceiling := range permissions {
		for requestedIndex, requested := range permissions {
			got := TightenPermission(requested, ceiling)
			want := requested
			if requestedIndex > ceilingIndex {
				want = ceiling
			}
			if got != want {
				t.Fatalf(
					"TightenPermission(%q, %q) = %q, want %q",
					requested, ceiling, got, want,
				)
			}
		}
	}
	if got := TightenPermission("unknown", PermissionBypass); got != PermissionNever {
		t.Fatalf("unknown requested posture = %q, want never", got)
	}
	if got := TightenPermission(PermissionBypass, "unknown"); got != PermissionNever {
		t.Fatalf("unknown ceiling = %q, want never", got)
	}
}

func TestApprovalCacheIsBoundedAndEvictsOldest(t *testing.T) {
	now := time.Unix(4000, 0)
	cache := NewApprovalCacheWithLimit(2)
	calls := make([]Invocation, 3)
	for index := range calls {
		calls[index] = invocation(
			"file_write", string(rune('a'+index)),
			`{"path":"`+string(rune('a'+index))+`"}`,
		)
		request, err := NewApprovalRequestForScope(
			calls[index],
			ApprovalSession,
			now.Add(time.Hour),
		)
		if err != nil {
			t.Fatal(err)
		}
		if err := cache.Add(request, ApprovalSession); err != nil {
			t.Fatal(err)
		}
	}
	if len(cache.entries) != 2 {
		t.Fatalf("cache size = %d, want 2", len(cache.entries))
	}
	if cache.MatchInvocation(calls[0], now) {
		t.Fatal("oldest approval remained after bounded eviction")
	}
	if !cache.MatchInvocation(calls[1], now) ||
		!cache.MatchInvocation(calls[2], now) {
		t.Fatal("new approvals were unexpectedly evicted")
	}
}

func invocation(toolName, callID, arguments string) Invocation {
	raw := json.RawMessage(arguments)
	capability := map[string]Capability{
		"file_read": CapabilityRead, "file_write": CapabilityWrite,
		"shell_read": CapabilityRead, "exec_command": CapabilityProcess,
		"request_user_input": CapabilityRead,
		"update_plan":        CapabilityWrite,
		"submit_plan":        CapabilityWrite,
	}[toolName]
	var resources []tool.Resource
	var value struct {
		Path string `json:"path"`
		CWD  string `json:"cwd"`
	}
	_ = json.Unmarshal(raw, &value)
	if value.Path != "" {
		resources = append(resources, tool.Resource{
			Kind: "file", Path: value.Path, Access: tool.AccessWrite,
		})
	}
	if toolName == "update_plan" || toolName == "submit_plan" {
		resources = append(resources, tool.Resource{
			Kind: "plan", ID: "session", Access: tool.AccessWrite,
		})
	}
	if value.CWD != "" {
		resources = append(resources, tool.Resource{
			Kind: "repo", Path: value.CWD, Access: tool.AccessWrite, Tree: true,
		})
	}
	return Invocation{
		CallID: callID, Tool: toolName, Arguments: raw,
		Resources: resources, Capability: capability,
		Access: map[string]tool.AccessMode{
			"file_read": tool.AccessRead, "file_write": tool.AccessWrite,
			"file_edit": tool.AccessWrite, "shell_read": tool.AccessRead,
			"exec_command": tool.AccessRead, "update_plan": tool.AccessWrite,
			"submit_plan": tool.AccessWrite,
		}[toolName],
		Sandbox: map[string]tool.SandboxRequirement{
			"file_read": tool.SandboxNone, "file_write": tool.SandboxNone,
			"file_edit": tool.SandboxNone, "shell_read": tool.SandboxStrong,
			"exec_command": tool.SandboxStrong, "update_plan": tool.SandboxNone,
			"submit_plan": tool.SandboxNone,
		}[toolName],
		Journaled: toolName == "file_write" || toolName == "file_edit",
		Validated: true,
	}
}

func authorize(runtime *Runtime, invocation Invocation) error {
	decision := runtime.Evaluate(invocation)
	if decision.Action == ActionAllow {
		return nil
	}
	return &DecisionError{Code: decision.Code, Reason: decision.Reason}
}

func assertDecisionCode(t *testing.T, err error, want string) {
	t.Helper()
	if want == "" {
		if err != nil {
			t.Fatalf("Authorize() error = %v", err)
		}
		return
	}
	var decision *DecisionError
	if !errors.As(err, &decision) || decision.Code != want {
		t.Fatalf("Authorize() error = %v, want code %s", err, want)
	}
}
