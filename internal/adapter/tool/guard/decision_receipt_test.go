package guard

import (
	"encoding/json"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/security/policy"
)

func TestPolicyDecisionIsRecordedInReceipts(t *testing.T) {
	t.Run("denial before any attempt", func(t *testing.T) {
		executor := &testExecutor{descriptor: writeDescriptor()}
		guard := newTestGuard(t, newTestRegistry(t, nil, executor),
			policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass), nil)
		result, err := guard.Execute(t.Context(), "denied-receipt", "write",
			json.RawMessage(`{"path":".qcode/permissions.toml","value":"x"}`))
		if err == nil || result.Execution == nil {
			t.Fatalf("Execute() = %+v, %v", result, err)
		}
		want := tool.PolicyDecisionReceipt{
			Action: string(policy.ActionDeny), Layer: string(policy.LayerHard),
			Code: "control_plane_protected",
		}
		if got := result.Execution.PolicyDenial; got == nil || *got != want ||
			len(result.Execution.Attempts) != 0 {
			t.Fatalf("receipt = %+v, denial = %+v", result.Execution, got)
		}
	})
	t.Run("admitted attempt", func(t *testing.T) {
		executor := &testExecutor{descriptor: readDescriptor("read")}
		guard := newTestGuard(t, newTestRegistry(t, nil, executor),
			policy.DefaultRuntime(policy.ModeAct, policy.PermissionSuggest), nil)
		result, err := guard.Execute(t.Context(), "admitted-receipt", "read", json.RawMessage(`{}`))
		if err != nil || result.Execution == nil || len(result.Execution.Attempts) != 1 {
			t.Fatalf("Execute() = %+v, %v", result, err)
		}
		want := tool.PolicyDecisionReceipt{
			Action: string(policy.ActionAllow), Layer: string(policy.LayerPosture),
		}
		if got := result.Execution.Attempts[0].Policy; got == nil || *got != want ||
			result.Execution.PolicyDenial != nil {
			t.Fatalf("attempt policy = %+v, denial = %+v", got, result.Execution.PolicyDenial)
		}
	})
}
