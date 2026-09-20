package guard

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/security/controlmatrix"
	"github.com/fwtllh-png/QCode/internal/security/egress"
	"github.com/fwtllh-png/QCode/internal/security/policy"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func TestProcessNetworkLeaseAggregatesLoopbackAndProxyTargets(t *testing.T) {
	for _, test := range []struct {
		name     string
		hosts    []string
		loopback bool
		deny     bool
		want     controlmatrix.Network
	}{
		{"host before localhost", []string{"api.github.com"}, true, false, controlmatrix.NetworkProxyTargets},
		{"host after localhost", []string{"registry.npmjs.org"}, true, false, controlmatrix.NetworkProxyTargets},
		{"targets ascending", []string{"api.github.com", "registry.npmjs.org"}, true, false, controlmatrix.NetworkProxyTargets},
		{"targets descending", []string{"registry.npmjs.org", "api.github.com"}, true, false, controlmatrix.NetworkProxyTargets},
		{"loopback only", nil, true, false, controlmatrix.NetworkLoopbackExact},
		{"proxy only", []string{"api.github.com"}, false, false, controlmatrix.NetworkProxyTargets},
		{"denied mixed request", []string{"api.github.com"}, true, true, controlmatrix.NetworkProxyTargets},
	} {
		t.Run(test.name, func(t *testing.T) {
			workspace := t.TempDir()
			const proxyPort = 43128 // Fake executor: no listener or external request is started.
			backend, err := sandbox.BindPolicy(networkAuthorityBackend{}, sandbox.Options{
				WorkspaceRoot: workspace, PrivateTemp: t.TempDir(),
				ManagedProxyPort: proxyPort, SkipPATHReadRoots: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			descriptor := processNetworkDescriptor()
			descriptor.ResourceResolver.LoopbackField = "allow_loopback"
			descriptor.InputSchema["properties"].(map[string]any)["allow_loopback"] =
				map[string]any{"type": "boolean"}
			executor := &testExecutor{descriptor: descriptor}
			registry := newTestRegistry(t, backend, executor)
			t.Cleanup(func() { _ = registry.Close() })
			runtime := policy.DefaultRuntime(policy.ModeAct, policy.PermissionSuggest)
			runtime.DisableAutoReview = true
			var granted []string
			guard, err := New(Options{
				Registry: registry, Policy: runtime, Workspace: workspace,
				OnNetworkAllow: func(capability tool.Capability, target egress.Target) {
					if capability != tool.CapabilityProcess {
						t.Errorf("grant capability = %q", capability)
					}
					granted = append(granted, target.Host)
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			approvals := 0
			guard.SetApprovalHandler(func(_ context.Context, request ApprovalRequest) error {
				approvals++
				if executor.calls.Load() != 0 || len(granted) != 0 {
					t.Error("execution or network grant occurred before approval")
				}
				if request.ReasonCode != ApprovalReasonNetworkHost {
					t.Errorf("approval reason = %q", request.ReasonCode)
				}
				loopback := false
				var hosts []string
				for _, resource := range request.Resources {
					if resource.Kind != "host" {
						continue
					}
					if resource.Protocol == "loopback" {
						loopback = true
					} else {
						hosts = append(hosts, resource.ID)
					}
				}
				wantHosts := slices.Clone(test.hosts)
				slices.Sort(wantHosts)
				slices.Sort(hosts)
				if loopback != test.loopback || !slices.Equal(hosts, wantHosts) {
					t.Errorf("approval omitted resources: %+v", request.Resources)
				}
				return guard.Decide(ApprovalDecision{
					RequestID: request.RequestID, Approved: !test.deny,
					Scope: policy.ApprovalOnce, ExpiresAt: request.ExpiresAt,
				})
			})
			targets := make([]map[string]any, 0, len(test.hosts))
			for _, host := range test.hosts {
				targets = append(targets, map[string]any{
					"host": host, "protocol": "https", "port": 443,
					"methods": []string{"CONNECT"}, "allow_private": false,
				})
			}
			raw, err := json.Marshal(map[string]any{
				"network_targets": targets, "allow_loopback": test.loopback,
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := guard.Execute(t.Context(), "network-lease", descriptor.Name, raw)
			if approvals != 1 || guard.Pending() != 0 {
				t.Fatalf("approvals=%d, pending=%d", approvals, guard.Pending())
			}
			if test.deny {
				var denied *policy.DecisionError
				if !errors.As(err, &denied) || denied.Code != "approval_denied" {
					t.Fatalf("denied execution error = %v", err)
				}
				if executor.calls.Load() != 0 || len(granted) != 0 || len(executor.profilesSnapshot()) != 0 {
					t.Fatal("denied request reached executor or granted network")
				}
				return
			}
			if err != nil {
				t.Fatalf("approved execution failed: %v", err)
			}
			profiles := executor.profilesSnapshot()
			if executor.calls.Load() != 1 || len(profiles) != 1 {
				t.Fatalf("calls=%d, profiles=%+v", executor.calls.Load(), profiles)
			}
			profile := profiles[0]
			wantPort := uint16(0)
			if len(test.hosts) != 0 {
				wantPort = proxyPort
			}
			if profile.RequiredControls.Network != test.want || profile.EffectiveControls.Network != test.want ||
				profile.AllowLoopback != test.loopback || profile.ManagedProxyPort != wantPort {
				t.Fatalf("execution authority = %+v", profile)
			}
			wantTargets := make([]string, 0, len(test.hosts)+1)
			for _, host := range test.hosts {
				wantTargets = append(wantTargets, "https://"+host+":443")
			}
			if test.loopback {
				wantTargets = append(wantTargets, "loopback://localhost:0")
			}
			slices.Sort(wantTargets)
			if !slices.Equal(profile.NetworkTargets, wantTargets) {
				t.Fatalf("network targets = %v, want %v", profile.NetworkTargets, wantTargets)
			}
			wantHosts := slices.Clone(test.hosts)
			slices.Sort(wantHosts)
			slices.Sort(granted)
			if !slices.Equal(granted, wantHosts) {
				t.Fatalf("granted hosts = %v, want %v", granted, wantHosts)
			}
			if result.Execution == nil || result.Execution.TerminalStatus != tool.OutcomeSucceeded ||
				len(result.Execution.Attempts) != 1 ||
				result.Execution.Attempts[0].EffectiveControls.Network != test.want ||
				result.Execution.Attempts[0].LoopbackAllowed != test.loopback {
				t.Fatalf("execution receipt = %+v", result.Execution)
			}
		})
	}
}

type networkAuthorityBackend struct{ strongBackend }

func (networkAuthorityBackend) Capability() sandbox.Capability {
	capability := (strongBackend{}).Capability()
	capability.ManagedProxy = true
	return capability
}
