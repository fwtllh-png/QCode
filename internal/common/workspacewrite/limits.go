// Package workspacewrite defines resource contracts shared by configuration,
// sandbox authorization and isolated workspace settlement.
package workspacewrite

// MaxDeclaredPaths is the published exec_command write_paths maxItems value.
// It bounds one authorization policy, not directory contents.
const MaxDeclaredPaths = 512

// DefaultMergeDiffBytes preserves the existing 3 MiB settlement-preview budget.
// execution.workspace_merge_max_diff_bytes exposes it to the operator.
const DefaultMergeDiffBytes = 3 << 20
