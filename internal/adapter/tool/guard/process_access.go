package guard

import (
	"encoding/json"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/security/policy"
)

// Full Access selects the process baseline. Resource declarations narrow their
// own dimensions during authority compilation; they never change this baseline.
// Discard retains the isolated, scoped execution contract.
// Recompute only this session fact when an approval wait changes permission;
// resolved paths and the rest of the assessment remain frozen.
func bindProcessAccess(invocation Invocation, runtime *policy.Runtime) Invocation {
	declared := invocation.Assessment.Declared()
	declared.FullAccess = false
	declared.HostExecution = false
	var target struct {
		ExecutionTarget string `json:"execution_target"`
	}
	if invocation.Binding.SupportsHostExecution &&
		tool.CatalogSourceKind(invocation.Tool, invocation.Ref.Source) == "builtin" &&
		json.Unmarshal(invocation.Arguments, &target) == nil && target.ExecutionTarget == "host" {
		declared.HostExecution = true
	}
	if runtime.Permission == policy.PermissionBypass && invocation.Binding.SupportsFullAccess &&
		!declared.HostExecution && tool.CatalogSourceKind(invocation.Tool, invocation.Ref.Source) == "builtin" {
		var input struct {
			Settle string `json:"settle"`
		}
		if json.Unmarshal(invocation.Arguments, &input) == nil && input.Settle != "discard" {
			declared.FullAccess = true
		}
	}
	invocation.Assessment = tool.AssessResources(invocation.Binding, declared, invocation.Resources)
	return invocation
}
