package guard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/security/policy"
)

func TestFreshApprovalExecutesOnceAndDoesNotAuthorizeNextCall(t *testing.T) {
	transports, approvals := 0, 0
	g, _ := networkFixture(t, nil, func(r *http.Request) (*http.Response, error) {
		transports++
		return networkResponse(r, ""), nil
	})
	g.Policy().SetForceEditPlanApproval(true)
	g.SetApprovalHandler(func(_ context.Context, request ApprovalRequest) error {
		approvals++
		if approvals > transports+1 {
			return fmt.Errorf("repeated approval before execution")
		}
		if request.ReplacementAllowed || len(request.AllowedScopes) != 1 || request.AllowedScopes[0] != policy.ApprovalOnce {
			return fmt.Errorf("fresh approval offered reusable or replacement authority")
		}
		return g.Decide(ApprovalDecision{RequestID: request.RequestID, Scope: policy.ApprovalOnce, Approved: true})
	})
	for i := 1; i <= 2; i++ {
		result, err := fetchNetwork(g, fmt.Sprintf("fresh-%d", i), "https://source.example/data")
		if err != nil || result.IsError || approvals != i || transports != i {
			t.Fatalf("call %d: approvals=%d transports=%d result=%+v err=%v", i, approvals, transports, result, err)
		}
	}
}

type countedExpansion struct {
	testExecutor
	expansions atomic.Int32
}

func (e *countedExpansion) ExpandArguments(_ context.Context, arguments json.RawMessage) (json.RawMessage, error) {
	e.expansions.Add(1)
	return arguments, nil
}

func TestApprovalReusesResolvedInputUntilArgumentsChange(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(fmt.Sprint(replace), func(t *testing.T) {
			descriptor := readDescriptor("frozen_input")
			descriptor.Capability = tool.CapabilityExternal
			descriptor.InputSchema["properties"] = map[string]any{"value": map[string]any{"type": "string"}}
			executor := &countedExpansion{testExecutor: testExecutor{descriptor: descriptor}}
			g := newTestGuard(t, newTestRegistry(t, nil, executor), policy.DefaultRuntime(policy.ModeAct, policy.PermissionSuggest), nil)
			approvals := 0
			g.SetApprovalHandler(func(_ context.Context, request ApprovalRequest) error {
				approvals++
				if approvals > 1 {
					return fmt.Errorf("duplicate approval")
				}
				answer := ApprovalDecision{RequestID: request.RequestID, Scope: policy.ApprovalOnce, Approved: true}
				if replace {
					answer.ReplacementArguments = json.RawMessage(`{"value":"changed"}`)
				}
				return g.Decide(answer)
			})
			prepared, err := g.authorize(t.Context(), "frozen-call", descriptor.Name, json.RawMessage(`{}`), tool.CatalogBinding{})
			if err != nil {
				t.Fatal(err)
			}
			want := int32(1)
			if replace {
				want = 2
			}
			if executor.expansions.Load() != want || !prepared.invocation.Assessment.Valid() {
				t.Fatalf("expansions=%d, want %d with a valid frozen assessment", executor.expansions.Load(), want)
			}
		})
	}
}
