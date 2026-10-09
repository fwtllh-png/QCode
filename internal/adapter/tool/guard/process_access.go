package guard

import (
	"encoding/json"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/security/policy"
)

// Explicit write scopes and discard retain the exact-path execution contract.
// Recompute only this session fact when an approval wait changes permission;
// resolved paths and the rest of the assessment remain frozen.
func bindProcessAccess(invocation Invocation, runtime *policy.Runtime) Invocation {
	declared := invocation.Assessment.Declared()
	declared.FullAccess = false
	if runtime.Permission == policy.PermissionBypass && invocation.Binding.SupportsFullAccess &&
		tool.CatalogSourceKind(invocation.Tool, invocation.Ref.Source) == "builtin" {
		var input struct {
			WritePaths     []string          `json:"write_paths"`
			Settle         string            `json:"settle"`
			NetworkTargets []json.RawMessage `json:"network_targets"`
			AllowLoopback  bool              `json:"allow_loopback"`
		}
		if json.Unmarshal(invocation.Arguments, &input) == nil && len(input.WritePaths) == 0 && input.Settle != "discard" && len(input.NetworkTargets) == 0 && !input.AllowLoopback {
			declared.FullAccess = true
		}
	}
	invocation.Assessment = tool.AssessResources(invocation.Binding, declared, invocation.Resources)
	return invocation
}
