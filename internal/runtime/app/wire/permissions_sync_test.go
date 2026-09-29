package wire

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	toolguard "github.com/fwtllh-png/QCode/internal/adapter/tool/guard"
	agentengine "github.com/fwtllh-png/QCode/internal/runtime/agent/engine"
	securitypolicy "github.com/fwtllh-png/QCode/internal/security/policy"
)

type permissionsSyncExecutor struct{}

func (permissionsSyncExecutor) Descriptor() tool.Descriptor {
	return tool.Descriptor{
		Name: "web_fetch", Description: "permission fixture", Visibility: tool.VisibleModel,
		Capability: tool.CapabilityNetwork, AccessMode: tool.AccessRead,
		ParallelPolicy: tool.ParallelConcurrent, SandboxRequirement: tool.SandboxNone,
		Availability: tool.AvailabilityAvailable,
		ResourceResolver: tool.ResourceResolver{Templates: []tool.ResourceTemplate{{
			Kind: "url", Field: "url", Access: tool.AccessRead,
		}}},
		InputSchema: map[string]any{
			"type": "object", "properties": map[string]any{"url": map[string]any{"type": "string"}},
			"required": []string{"url"}, "additionalProperties": false,
		},
	}
}

func (permissionsSyncExecutor) Execute(context.Context, json.RawMessage) (tool.Result, error) {
	return tool.Result{Content: "ok"}, nil
}

func TestAlwaysPermissionSharedAcrossWorkspaceGuards(t *testing.T) {
	for _, scope := range []securitypolicy.ApprovalScope{securitypolicy.ApprovalAlways, securitypolicy.ApprovalSession, securitypolicy.ApprovalOnce} {
		t.Run(string(scope), func(t *testing.T) {
			root := t.TempDir()
			store, err := securitypolicy.OpenPermissionsStore(filepath.Join(t.TempDir(), securitypolicy.PermissionsFileName))
			if err != nil {
				t.Fatal(err)
			}
			registry := tool.NewRegistry(nil, nil)
			t.Cleanup(func() { _ = registry.Close() })
			if err := registry.Register(permissionsSyncExecutor{}); err != nil {
				t.Fatal(err)
			}
			seed := securitypolicy.DefaultRuntime(securitypolicy.ModeAct, securitypolicy.PermissionSuggest)
			seed.SetDisableAutoReview(true)
			base := guardFactory{registry: registry, runtime: seed, permissions: store, workspace: root}
			build := func(runtime *securitypolicy.Runtime, workspace string) *toolguard.Guard {
				options := agentengine.Options{}
				options.Tools, options.Security, options.Workspace = registry, runtime, workspace
				bindEngineGuardFactory(&options, base, nil)
				g, err := options.GuardFactory(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				return g
			}
			a := build(cloneThreadSecurity(seed), root)
			b := build(cloneThreadSecurity(seed), root)
			other := build(cloneThreadSecurity(seed), t.TempDir())
			managed := cloneThreadSecurity(seed)
			if _, err := managed.AppendManagedRule(securitypolicy.Rule{
				Tool: "web_fetch", Action: securitypolicy.ActionDeny, Code: "child_authority_denied",
			}); err != nil {
				t.Fatal(err)
			}
			repository := cloneThreadSecurity(seed)
			if _, err := repository.ReloadSources(nil, []securitypolicy.Rule{{
				Tool: "web_fetch", Action: securitypolicy.ActionDeny,
			}}); err != nil {
				t.Fatal(err)
			}
			readOnly := cloneThreadSecurity(seed)
			readOnly.SetPermission(securitypolicy.PermissionNever)
			restricted := map[*toolguard.Guard]string{
				build(managed, root):    "tool_grant_denied",
				build(repository, root): "repository_rule_denied",
				build(readOnly, root):   "permission_denied",
			}
			counts := map[*toolguard.Guard]int{}
			execute := func(g *toolguard.Guard, id string, selected securitypolicy.ApprovalScope) {
				g.SetApprovalHandler(func(_ context.Context, request toolguard.ApprovalRequest) error {
					counts[g]++
					return g.Decide(toolguard.ApprovalDecision{
						RequestID: request.RequestID, Approved: true, Scope: selected,
					})
				})
				result, err := g.Execute(t.Context(), id, "web_fetch",
					json.RawMessage(`{"url":"https://example.com/a"}`))
				if err != nil || result.IsError {
					t.Fatalf("execute %s: err=%v result=%+v", id, err, result)
				}
			}
			frozen := b.Policy().CloneSampling()
			execute(a, "a-initial", scope)
			execute(b, "b-existing", securitypolicy.ApprovalOnce)
			c := build(cloneThreadSecurity(seed), root)
			execute(c, "c-new", securitypolicy.ApprovalOnce)
			execute(other, "other-workspace", securitypolicy.ApprovalOnce)
			execute(a, "a-repeat", securitypolicy.ApprovalOnce)
			wantOthers, wantOwn, wantRules := 1, 1, 0
			if scope == securitypolicy.ApprovalAlways {
				wantOthers, wantRules = 0, 1
			}
			if scope == securitypolicy.ApprovalOnce {
				wantOwn = 2
			}
			if counts[a] != wantOwn || counts[b] != wantOthers || counts[c] != wantOthers ||
				counts[other] != 1 || len(store.Rules()) != wantRules {
				t.Fatalf("approvals: own=%d existing=%d new=%d other=%d rules=%d",
					counts[a], counts[b], counts[c], counts[other], len(store.Rules()))
			}
			if len(frozen.User) != 0 {
				t.Fatal("persistent update changed an already frozen policy snapshot")
			}
			if scope == securitypolicy.ApprovalAlways && b.Policy().CloneSampling().Revision <= frozen.Revision {
				t.Fatal("workspace permission update did not advance policy revision")
			}
			for g, code := range restricted {
				_, err := g.Execute(t.Context(), code, "web_fetch",
					json.RawMessage(`{"url":"https://example.com/a"}`))
				var denied *securitypolicy.DecisionError
				if !errors.As(err, &denied) || denied.Code != code {
					t.Fatalf("restriction %s changed after approval: %v", code, err)
				}
			}
		})
	}
}
