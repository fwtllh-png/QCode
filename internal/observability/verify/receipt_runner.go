package verify

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// ReceiptRunner consumes already executed evidence. It never starts a process.
type ReceiptRunner struct {
	Root    string
	Command string
}

func (r *ReceiptRunner) Verify(_ context.Context, request Request) (Receipt, error) {
	switch request.Scope {
	case ScopeDiagnostics, ScopeRepository, ScopeAffected, ScopeCommands:
	default:
		return Receipt{}, fmt.Errorf("unknown verify scope %q", request.Scope)
	}
	if request.RequireConfiguredCommand && strings.TrimSpace(r.Command) == "" {
		return Receipt{Scope: request.Scope, Status: StatusUnavailable,
			UncoveredPaths: append([]string(nil), request.Paths...),
			Message:        "hard verification requires execution.verify.command to define the acceptance check; configure a reproducible project check or use soft mode"}, nil
	}
	if r.Root != "" {
		request.Paths = relativePaths(r.Root, request.Paths)
	}
	command := expandCommand(r.Command, request.Paths)
	latest := make(map[string]int)
	for index, evidence := range request.Evidence {
		if evidence.MutationRevision != request.MutationRevision ||
			evidence.WorkspaceRevision != request.WorkspaceRevision {
			continue
		}
		if command != "" && (evidence.Command != command || evidence.CWD != "" && evidence.CWD != ".") {
			continue
		}
		latest[evidenceKey(evidence)] = index
	}
	inputs := make([]Evidence, 0, len(latest))
	for _, index := range slices.Sorted(maps.Values(latest)) {
		evidence := request.Evidence[index]
		if r.Root != "" && evidence.Status == StatusPassed {
			digest, err := InputDigest(r.Root, evidence.CoveredPaths)
			if err != nil || evidence.InputDigest == "" || digest != evidence.InputDigest {
				evidence.Status = StatusUnavailable
			}
		}
		inputs = append(inputs, evidence)
	}
	if command != "" || len(inputs) != 0 {
		receipt, _ := CommandEvidenceReceipt(request.Paths, request.MutationRevision, inputs)
		if command != "" && len(inputs) == 0 {
			receipt.Status = StatusUnavailable
		}
		if command != "" && receipt.Status != StatusPassed {
			receipt.Message += "; run via exec_command with verification and covered_paths: " + command
		}
		return receipt, nil
	}
	if request.Scope == ScopeDiagnostics {
		return FromDiagnostics(request.Diagnostics, request.Paths), nil
	}
	return Receipt{
		Scope: ScopeCommands, Status: StatusUnavailable,
		UncoveredPaths: append([]string(nil), request.Paths...),
		Message:        "no current verification execution evidence; run the project's verification command via exec_command with verification and covered_paths",
	}, nil
}
