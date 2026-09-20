package engine

import (
	"context"
	"encoding/json"
	"net"
	"sync/atomic"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/security/egress"
	"github.com/fwtllh-png/QCode/internal/security/policy"
)

// The engine-owned Guard must make approvals usable by the current HTTP
// attempt without granting the target to unrelated calls.
func TestAllocatedGuardGrantsEgressAfterApproval(t *testing.T) {
	registry := tool.NewRegistry(nil, nil)
	executor := &egressRetryTool{}
	if err := registry.Register(executor); err != nil {
		t.Fatal(err)
	}

	engine, err := newTestEngine(Options{ProviderConfig: ProviderConfig{Provider: &scriptedProvider{}, Route: testRoute(t)}, ToolConfig: ToolConfig{Tools: registry,

		OnNetworkAllow: func(capability tool.Capability, target egress.Target) {
			t.Errorf("Web grant escaped call scope: %q %+v", capability, target)
		}}, SecurityConfig: SecurityConfig{Workspace: t.TempDir(),
		Security: policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass)},
	})
	if err != nil {
		t.Fatal(err)
	}

	result, execErr := engine.guard.Execute(
		context.Background(), "call-egress", "web_fetch",
		json.RawMessage(`{"url":"https://example.com/page"}`),
	)
	if execErr != nil {
		t.Fatal(execErr)
	}
	if result.IsError || result.Content != `{"ok":true}` {
		t.Fatalf("result = %+v", result)
	}
	if executor.calls.Load() != 2 {
		t.Fatalf("calls = %d, want retry after grant", executor.calls.Load())
	}
}

type egressRetryTool struct {
	calls atomic.Int32
}

func (e *egressRetryTool) Descriptor() tool.Descriptor {
	return tool.Descriptor{
		Name: "web_fetch", Description: "test fetch", Visibility: tool.VisibleModel,
		Capability: tool.CapabilityNetwork, AccessMode: tool.AccessRead,
		ResourceResolver: tool.ResourceResolver{Templates: []tool.ResourceTemplate{{
			Kind: "url", Field: "url", Access: tool.AccessRead,
		}}},
		ParallelPolicy: tool.ParallelConcurrent, SandboxRequirement: tool.SandboxNone,
		Availability: tool.AvailabilityAvailable,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url": map[string]any{"type": "string", "minLength": 1},
			},
			"required": []string{"url"}, "additionalProperties": false,
		},
	}
}

func (e *egressRetryTool) Execute(ctx context.Context, _ json.RawMessage) (tool.Result, error) {
	if e.calls.Add(1) == 1 {
		return tool.Result{
			Content: "egress denied · host=cdn.example", IsError: true,
			Metadata: map[string]any{
				"error_category": "egress_denied", "host": "cdn.example",
				"protocol": "https", "status_code": 0,
			},
			Outcome: &tool.Outcome{
				Status: tool.OutcomeFailed,
				Security: &tool.SecuritySignal{
					EgressDenied: &tool.NetworkTarget{
						Host: "cdn.example", Protocol: "https",
					},
				},
			},
		}, nil
	}
	gate := &egress.Gate{
		Enforce: true, UseCallScope: true,
		LookupIP: func(context.Context, string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("203.0.113.10")}, nil
		},
	}
	if _, err := gate.Authorize(ctx, egress.Target{Host: "cdn.example", Protocol: "https"}, "test"); err != nil {
		return tool.Result{}, err
	}
	return tool.Result{Content: `{"ok":true}`}, nil
}
